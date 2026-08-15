// Package ledger implements application-level ledger workflows.
package ledger

import (
	"context"
	"database/sql"
	"fmt"

	"tokenledger/internal/database/postgres"
	"tokenledger/internal/domain"
)

// Service coordinates repositories inside the transaction boundaries required
// by the ledger. Posting workflows will be added to this same service.
type Service struct {
	store  *postgres.Store
	owners postgres.OwnerRepository
	ledger postgres.LedgerRepository
}

type TransactionResult struct {
	TransactionID    int64 `json:"transaction_id"`
	OwnerID          int64 `json:"owner_id"`
	AvailableBalance int64 `json:"available_balance"`
}
type DepositCommand struct {
	OwnerID     int64
	Amount      domain.Amount
	Description string
	Key         domain.IdempotencyKey
	Metadata    domain.Metadata
}
type SpendCommand DepositCommand
type AdjustCommand struct {
	OwnerID     int64
	Description string
	Key         domain.IdempotencyKey
	Metadata    domain.Metadata
	Postings    []domain.Posting
}
type RegisterSourceCommand struct {
	Name        string
	DisplayName string
	Metadata    domain.Metadata
}
type TransactionPage struct {
	Items []domain.Transaction
	Next  *postgres.PageCursor
}

func NewService(store *postgres.Store) *Service {
	return &Service{store: store}
}

func (s *Service) Deposit(ctx context.Context, command DepositCommand) (TransactionResult, error) {
	if err := command.Amount.Validate(); err != nil {
		return TransactionResult{}, err
	}
	source := "other"
	if command.Key.Source != "" {
		source = command.Key.Source
	}
	sourceCode, err := SourceAccountCode(source)
	if err != nil {
		return TransactionResult{}, err
	}
	return s.post(ctx, domain.Deposit, command.OwnerID, command.Description, command.Key, command.Metadata, []domain.Posting{
		{AccountCode: "", AccountName: "Wallet", Side: domain.Debit, Amount: command.Amount, Metadata: domain.EmptyMetadata()},
		{AccountCode: sourceCode, AccountName: postgres.SystemAccountName(sourceCode), Side: domain.Credit, Amount: command.Amount, Metadata: domain.EmptyMetadata()},
	})
}

func (s *Service) Spend(ctx context.Context, command SpendCommand) (TransactionResult, error) {
	if err := command.Amount.Validate(); err != nil {
		return TransactionResult{}, err
	}
	return s.post(ctx, domain.Spend, command.OwnerID, command.Description, command.Key, command.Metadata, []domain.Posting{
		{AccountCode: "", AccountName: "Wallet", Side: domain.Credit, Amount: command.Amount, Metadata: domain.EmptyMetadata(), EnforcePositive: true},
		{AccountCode: "sink:spend", AccountName: "sink spend", Side: domain.Debit, Amount: command.Amount, Metadata: domain.EmptyMetadata()},
	})
}

func (s *Service) Adjust(ctx context.Context, command AdjustCommand) (TransactionResult, error) {
	return s.post(ctx, domain.Adjustment, command.OwnerID, command.Description, command.Key, command.Metadata, command.Postings)
}

func (s *Service) post(ctx context.Context, typ domain.TransactionType, ownerID int64, description string, key domain.IdempotencyKey, metadata domain.Metadata, postings []domain.Posting) (result TransactionResult, err error) {
	if ownerID <= 0 || description == "" {
		return result, fmt.Errorf("%w: owner ID and description are required", domain.ErrInvalidTransaction)
	}
	if err := key.Validate(); err != nil {
		return result, err
	}
	if err := metadata.Validate(); err != nil {
		return result, err
	}
	walletCode, err := WalletAccountCode(ownerID)
	if err != nil {
		return result, err
	}
	for i := range postings {
		if postings[i].AccountCode == "" {
			postings[i].AccountCode = walletCode
			postings[i].AccountName = fmt.Sprintf("Owner %d Wallet", ownerID)
		}
	}
	if err := domain.ValidatePostings(postings); err != nil {
		return result, err
	}
	return result, s.store.WithinTransaction(ctx, func(tx *sql.Tx) error {
		owner, err := s.owners.GetForUpdate(ctx, tx, ownerID)
		if err != nil {
			return err
		}
		ownerRef := domain.OwnerRef{Type: string(owner.Type), ID: owner.ID}
		specs := make([]postgres.AccountSpec, 0, len(postings))
		for _, posting := range postings {
			var accountOwner *int64
			if posting.AccountCode == walletCode {
				accountOwner = &ownerID
			}
			if err := ValidateAccountCodeOwnership(posting.AccountCode, accountOwner); err != nil {
				return err
			}
			specs = append(specs, postgres.AccountSpec{Code: posting.AccountCode, Name: posting.AccountName, OwnerID: accountOwner, Metadata: posting.Metadata})
		}
		accounts, err := s.ledger.LockExistingAccounts(ctx, tx, specs)
		if err != nil {
			return err
		}
		balances := make(map[string]int64, len(accounts))
		for code, account := range accounts {
			balances[code] = account.CurrentBalance
		}
		for _, posting := range postings {
			balances[posting.AccountCode] += postgres.AccountDelta(posting.Side, posting.Amount)
		}
		for _, posting := range postings {
			if posting.EnforcePositive && balances[posting.AccountCode] < 0 {
				return domain.ErrInsufficientFunds
			}
		}
		transaction, err := s.ledger.InsertTransaction(ctx, tx, postgres.NewTransaction{Type: typ, Description: description, Owner: ownerRef, Key: key, Metadata: metadata})
		if err != nil {
			return err
		}
		if err := s.ledger.InsertEntries(ctx, tx, transaction.ID, postings, accounts); err != nil {
			return err
		}
		for code, account := range accounts {
			if err := s.ledger.UpdateAccountBalance(ctx, tx, account.ID, balances[code]); err != nil {
				return err
			}
		}
		if err := s.owners.SetCachedBalance(ctx, tx, ownerID, balances[walletCode]); err != nil {
			return err
		}
		result = TransactionResult{TransactionID: transaction.ID, OwnerID: ownerID, AvailableBalance: balances[walletCode]}
		return nil
	})
}

func (s *Service) GetBalance(ctx context.Context, ownerID int64) (int64, error) {
	owner, err := s.owners.Get(ctx, s.store.DB(), ownerID)
	return owner.CachedBalance, err
}
func (s *Service) GetTransaction(ctx context.Context, id int64) (postgres.TransactionRecord, error) {
	return s.ledger.GetTransaction(ctx, s.store.DB(), id)
}
func (s *Service) ListTransactions(ctx context.Context, ownerID int64, limit int, cursor *postgres.PageCursor) (TransactionPage, error) {
	if _, err := s.owners.Get(ctx, s.store.DB(), ownerID); err != nil {
		return TransactionPage{}, err
	}
	items, err := s.ledger.ListTransactions(ctx, s.store.DB(), ownerID, limit+1, cursor)
	if err != nil {
		return TransactionPage{}, err
	}
	page := TransactionPage{Items: items}
	if len(page.Items) > limit {
		final := page.Items[limit-1]
		page.Next = &postgres.PageCursor{CreatedAt: final.CreatedAt, ID: final.ID}
		page.Items = page.Items[:limit]
	}
	return page, nil
}
func (s *Service) ReconcileOwner(ctx context.Context, ownerID int64) (previous, balance int64, repaired bool, err error) {
	err = s.store.WithinTransaction(ctx, func(tx *sql.Tx) error {
		owner, err := s.owners.GetForUpdate(ctx, tx, ownerID)
		if err != nil {
			return err
		}
		previous = owner.CachedBalance
		balance, err = s.ledger.CalculateWalletBalance(ctx, tx, ownerID)
		if err != nil {
			return err
		}
		walletCode, err := WalletAccountCode(ownerID)
		if err != nil {
			return err
		}
		accounts, err := s.ledger.LockExistingAccounts(ctx, tx, []postgres.AccountSpec{{Code: walletCode, Name: fmt.Sprintf("Owner %d Wallet", ownerID), OwnerID: &ownerID, Metadata: domain.EmptyMetadata()}})
		if err != nil {
			return err
		}
		if err = s.ledger.UpdateAccountBalance(ctx, tx, accounts[walletCode].ID, balance); err != nil {
			return err
		}
		if err = s.owners.SetCachedBalance(ctx, tx, ownerID, balance); err != nil {
			return err
		}
		repaired = previous != balance
		return nil
	})
	return
}

type CreateOwnerCommand struct {
	Type        domain.OwnerType
	ExternalRef string
	DisplayName string
	Metadata    domain.Metadata
}

func (s *Service) CreateOwner(ctx context.Context, command CreateOwnerCommand) (domain.Owner, error) {
	owner := domain.Owner{
		Type:        command.Type,
		ExternalRef: command.ExternalRef,
		DisplayName: command.DisplayName,
		Metadata:    command.Metadata,
	}
	if err := owner.Validate(); err != nil {
		return domain.Owner{}, err
	}
	if err := s.store.WithinTransaction(ctx, func(tx *sql.Tx) error {
		created, err := s.owners.Create(ctx, tx, owner)
		if err != nil {
			return err
		}
		owner = created
		walletCode, err := WalletAccountCode(owner.ID)
		if err != nil {
			return err
		}
		_, err = s.ledger.CreateAccount(ctx, tx, postgres.AccountSpec{Code: walletCode, Name: fmt.Sprintf("Owner %d Wallet", owner.ID), OwnerID: &owner.ID, Metadata: domain.EmptyMetadata()})
		return err
	}); err != nil {
		return domain.Owner{}, fmt.Errorf("create owner: %w", err)
	}
	return owner, nil
}

// RegisterSource creates one system-owned source account. Deposits only accept
// sources registered through this trusted operator workflow.
func (s *Service) RegisterSource(ctx context.Context, command RegisterSourceCommand) (domain.Account, error) {
	code, err := SourceAccountCode(command.Name)
	if err != nil {
		return domain.Account{}, err
	}
	if command.DisplayName == "" {
		return domain.Account{}, fmt.Errorf("%w: source display name is required", domain.ErrInvalidPosting)
	}
	if err := command.Metadata.Validate(); err != nil {
		return domain.Account{}, err
	}
	var account domain.Account
	err = s.store.WithinTransaction(ctx, func(tx *sql.Tx) error {
		created, err := s.ledger.CreateAccount(ctx, tx, postgres.AccountSpec{Code: code, Name: command.DisplayName, Metadata: command.Metadata})
		if err != nil {
			return err
		}
		account = created
		return nil
	})
	if err != nil {
		return domain.Account{}, fmt.Errorf("register source: %w", err)
	}
	return account, nil
}
