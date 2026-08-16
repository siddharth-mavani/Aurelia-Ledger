package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"tokenledger/internal/domain"
)

// LedgerRepository contains persistence operations for accounts and journal rows.
// Write methods require the transaction owned by the service's posting workflow.
type LedgerRepository struct{}

type AccountSpec struct {
	Code     string
	Name     string
	OwnerID  *int64
	Metadata domain.Metadata
}

func (LedgerRepository) CreateAccount(ctx context.Context, tx *sql.Tx, spec AccountSpec) (domain.Account, error) {
	if spec.Code == "" || spec.Name == "" {
		return domain.Account{}, fmt.Errorf("%w: account code and name are required", domain.ErrInvalidPosting)
	}
	var account domain.Account
	err := tx.QueryRowContext(ctx, `
		INSERT INTO ledger_accounts (code, name, owner_id, metadata)
		VALUES ($1, $2, $3, $4)
		RETURNING id, code, name, current_balance, metadata, created_at, updated_at`,
		spec.Code, spec.Name, spec.OwnerID, normalizeMetadata(spec.Metadata),
	).Scan(&account.ID, &account.Code, &account.Name, &account.CurrentBalance, &account.Metadata, &account.CreatedAt, &account.UpdatedAt)
	if isUniqueViolation(err) {
		return domain.Account{}, fmt.Errorf("%w: %s", domain.ErrDuplicateAccount, spec.Code)
	}
	if err != nil {
		return domain.Account{}, fmt.Errorf("create account %q: %w", spec.Code, err)
	}
	return account, nil
}

// LockExistingAccounts locks every supplied account in deterministic code order.
// It never creates an account; callers must provision accounts explicitly.
func (LedgerRepository) LockExistingAccounts(ctx context.Context, tx *sql.Tx, specs []AccountSpec) (map[string]domain.Account, error) {
	byCode := make(map[string]AccountSpec, len(specs))
	for _, spec := range specs {
		if spec.Code == "" || spec.Name == "" {
			return nil, fmt.Errorf("%w: account code and name are required", domain.ErrInvalidPosting)
		}
		if old, exists := byCode[spec.Code]; exists {
			if (old.OwnerID == nil) != (spec.OwnerID == nil) || (old.OwnerID != nil && *old.OwnerID != *spec.OwnerID) {
				return nil, fmt.Errorf("%w: inconsistent ownership for %q", domain.ErrInvalidPosting, spec.Code)
			}
		}
		byCode[spec.Code] = spec
	}
	codes := make([]string, 0, len(byCode))
	for code := range byCode {
		codes = append(codes, code)
	}
	sort.Strings(codes)

	accounts := make(map[string]domain.Account, len(codes))
	for _, code := range codes {
		var account domain.Account
		var ownerID sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			SELECT id, code, name, current_balance, metadata, created_at, updated_at, owner_id
			FROM ledger_accounts WHERE code = $1 FOR UPDATE`, code,
		).Scan(&account.ID, &account.Code, &account.Name, &account.CurrentBalance, &account.Metadata, &account.CreatedAt, &account.UpdatedAt, &ownerID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("%w: %s", domain.ErrAccountNotFound, code)
			}
			return nil, fmt.Errorf("lock account %q: %w", code, err)
		}
		spec := byCode[code]
		if (spec.OwnerID == nil) != !ownerID.Valid || (spec.OwnerID != nil && ownerID.Int64 != *spec.OwnerID) {
			return nil, fmt.Errorf("%w: account %q has incompatible owner", domain.ErrInvalidPosting, code)
		}
		accounts[code] = account
	}
	return accounts, nil
}

func (LedgerRepository) UpdateAccountBalance(ctx context.Context, tx *sql.Tx, accountID, balance int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE ledger_accounts SET current_balance = $2, updated_at = now() WHERE id = $1`, accountID, balance); err != nil {
		return fmt.Errorf("update account balance: %w", err)
	}
	return nil
}

type NewTransaction struct {
	Type                domain.TransactionType
	Description         string
	Owner               domain.OwnerRef
	ParentTransactionID *int64
	Key                 domain.IdempotencyKey
	Metadata            domain.Metadata
}

func (LedgerRepository) InsertTransaction(ctx context.Context, tx *sql.Tx, item NewTransaction) (domain.Transaction, error) {
	var result domain.Transaction
	if err := item.Type.Validate(); err != nil {
		return result, err
	}
	if item.Description == "" || !item.Owner.Valid() {
		return result, fmt.Errorf("%w: description and owner are required", domain.ErrInvalidTransaction)
	}
	if err := item.Key.Validate(); err != nil {
		return result, err
	}
	if err := item.Metadata.Validate(); err != nil {
		return result, err
	}
	result.Type, result.Description = item.Type, item.Description
	result.Owner = &item.Owner
	result.IdempotencyKey, result.Metadata = item.Key, normalizeMetadata(item.Metadata)
	err := tx.QueryRowContext(ctx, `
		INSERT INTO ledger_transactions (transaction_type, description, owner_type, owner_id, parent_transaction_id, external_source, external_id, metadata)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), $8)
		RETURNING id, created_at, updated_at`, item.Type, item.Description, item.Owner.Type, item.Owner.ID, item.ParentTransactionID, item.Key.Source, item.Key.ID, result.Metadata,
	).Scan(&result.ID, &result.CreatedAt, &result.UpdatedAt)
	if isUniqueViolation(err) {
		return domain.Transaction{}, fmt.Errorf("%w: %s/%s", domain.ErrDuplicateTransaction, item.Key.Source, item.Key.ID)
	}
	if err != nil {
		return domain.Transaction{}, fmt.Errorf("insert transaction: %w", err)
	}
	return result, nil
}

func (LedgerRepository) InsertEntries(ctx context.Context, tx *sql.Tx, transactionID int64, postings []domain.Posting, accounts map[string]domain.Account) error {
	if transactionID <= 0 {
		return fmt.Errorf("%w: transaction ID must be positive", domain.ErrInvalidTransaction)
	}
	if err := domain.ValidatePostings(postings); err != nil {
		return err
	}
	for _, posting := range postings {
		account, ok := accounts[posting.AccountCode]
		if !ok {
			return fmt.Errorf("%w: %s", domain.ErrAccountNotFound, posting.AccountCode)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ledger_entries (account_id, transaction_id, entry_type, amount, metadata)
			VALUES ($1, $2, $3, $4, $5)`, account.ID, transactionID, posting.Side, posting.Amount, normalizeMetadata(posting.Metadata)); err != nil {
			return fmt.Errorf("insert entry: %w", err)
		}
	}
	return nil
}

type TransactionRecord struct {
	Transaction domain.Transaction `json:"transaction"`
	Entries     []domain.Entry     `json:"entries"`
}

func (LedgerRepository) GetTransaction(ctx context.Context, db queryExecutor, id int64) (TransactionRecord, error) {
	var record TransactionRecord
	var ownerType sql.NullString
	var ownerID sql.NullInt64
	var source, externalID sql.NullString
	var parentID sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT id, transaction_type, description, owner_type, owner_id, parent_transaction_id, external_source, external_id, metadata, created_at, updated_at FROM ledger_transactions WHERE id = $1`, id).
		Scan(&record.Transaction.ID, &record.Transaction.Type, &record.Transaction.Description, &ownerType, &ownerID, &parentID, &source, &externalID, &record.Transaction.Metadata, &record.Transaction.CreatedAt, &record.Transaction.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return record, fmt.Errorf("%w: %d", domain.ErrTransactionNotFound, id)
	}
	if err != nil {
		return record, fmt.Errorf("get transaction: %w", err)
	}
	if ownerID.Valid {
		record.Transaction.Owner = &domain.OwnerRef{Type: ownerType.String, ID: ownerID.Int64}
	}
	if parentID.Valid {
		record.Transaction.ParentTransactionID = &parentID.Int64
	}
	record.Transaction.IdempotencyKey = domain.IdempotencyKey{Source: source.String, ID: externalID.String}
	rows, err := db.QueryContext(ctx, `SELECT id, account_id, transaction_id, entry_type, amount, metadata, created_at, updated_at FROM ledger_entries WHERE transaction_id = $1 ORDER BY id`, id)
	if err != nil {
		return record, err
	}
	defer rows.Close()
	for rows.Next() {
		var entry domain.Entry
		if err := rows.Scan(&entry.ID, &entry.AccountID, &entry.TransactionID, &entry.Side, &entry.Amount, &entry.Metadata, &entry.CreatedAt, &entry.UpdatedAt); err != nil {
			return record, err
		}
		record.Entries = append(record.Entries, entry)
	}
	return record, rows.Err()
}

type PageCursor struct {
	CreatedAt time.Time
	ID        int64
}

func (LedgerRepository) ListTransactions(ctx context.Context, db queryExecutor, ownerID int64, limit int, cursor *PageCursor) ([]domain.Transaction, error) {
	query := `SELECT id, transaction_type, description, owner_type, owner_id, parent_transaction_id, external_source, external_id, metadata, created_at, updated_at FROM ledger_transactions WHERE owner_id = $1`
	args := []any{ownerID}
	if cursor != nil {
		query += ` AND (created_at, id) < ($2, $3)`
		args = append(args, cursor.CreatedAt, cursor.ID)
	}
	query += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []domain.Transaction
	for rows.Next() {
		var item domain.Transaction
		var typ sql.NullString
		var oid, parentID sql.NullInt64
		var source, ext sql.NullString
		if err := rows.Scan(&item.ID, &item.Type, &item.Description, &typ, &oid, &parentID, &source, &ext, &item.Metadata, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		if oid.Valid {
			item.Owner = &domain.OwnerRef{Type: typ.String, ID: oid.Int64}
		}
		if parentID.Valid {
			item.ParentTransactionID = &parentID.Int64
		}
		item.IdempotencyKey = domain.IdempotencyKey{Source: source.String, ID: ext.String}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (LedgerRepository) CalculateWalletBalance(ctx context.Context, tx *sql.Tx, ownerID int64) (int64, error) {
	var balance int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE entry_type WHEN 'debit' THEN amount ELSE -amount END), 0) FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id WHERE a.owner_id = $1 AND a.code = 'wallet:' || $1::text`, ownerID).Scan(&balance)
	return balance, err
}

// queryExecutor is the complete read capability needed by history queries.
// *sql.DB and *sql.Tx both implement it, so callers can use the same methods
// inside or outside a database transaction without runtime type assertions.
type queryExecutor interface {
	singleRowQuerier
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func AccountDelta(side domain.EntrySide, amount domain.Amount) int64 {
	if side == domain.Debit {
		return int64(amount)
	}
	return -int64(amount)
}
func SystemAccountName(code string) string { return strings.ReplaceAll(code, ":", " ") }
