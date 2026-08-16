// Package ledger implements application-level ledger workflows.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"aurelialedger/internal/database/postgres"
	"aurelialedger/internal/domain"
)

// Service coordinates repositories inside the transaction boundaries required
// by the ledger. Posting workflows will be added to this same service.
type Service struct {
	store        *postgres.Store
	owners       postgres.OwnerRepository
	ledger       postgres.LedgerRepository
	reservations postgres.ReservationRepository
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
type ReserveCommand DepositCommand
type SettlementCommand struct {
	ReservationID int64
	Amount        *domain.Amount // required for capture; nil release means the complete remaining amount.
	Description   string
	Key           domain.IdempotencyKey
	Metadata      domain.Metadata
}

// ExternalWork runs after a reservation commits and before it is settled.
type ExternalWork interface{ Execute(context.Context) error }
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

func (s *Service) Reserve(ctx context.Context, command ReserveCommand) (postgres.Reservation, error) {
	if err := command.Amount.Validate(); err != nil {
		return postgres.Reservation{}, err
	}
	if err := command.Key.Validate(); err != nil {
		return postgres.Reservation{}, err
	}
	if command.OwnerID <= 0 || command.Description == "" {
		return postgres.Reservation{}, fmt.Errorf("%w: owner ID and description are required", domain.ErrInvalidTransaction)
	}
	var reservation postgres.Reservation
	result, err := s.postWithParent(ctx, domain.Reserve, command.OwnerID, command.Description, command.Key, command.Metadata, nil, []domain.Posting{
		{AccountCode: "", AccountName: "Wallet", Side: domain.Credit, Amount: command.Amount, Metadata: domain.EmptyMetadata(), EnforcePositive: true},
		{AccountCode: reservedCode(command.OwnerID), AccountName: "Reserved Wallet", Side: domain.Debit, Amount: command.Amount, Metadata: domain.EmptyMetadata()},
	}, func(tx *sql.Tx, result TransactionResult) error {
		var e error
		reservation, e = s.reservations.Create(ctx, tx, result.TransactionID, command.OwnerID, command.Amount)
		return e
	})
	_ = result
	return reservation, err
}

func reservedCode(ownerID int64) string { code, _ := ReservedAccountCode(ownerID); return code }

func (s *Service) Capture(ctx context.Context, command SettlementCommand) (postgres.Reservation, error) {
	if command.Amount == nil {
		return postgres.Reservation{}, fmt.Errorf("%w: capture amount is required", domain.ErrInvalidAmount)
	}
	return s.settle(ctx, command, true)
}
func (s *Service) Release(ctx context.Context, command SettlementCommand) (postgres.Reservation, error) {
	return s.settle(ctx, command, false)
}
func (s *Service) GetReservation(ctx context.Context, reservationID int64) (postgres.Reservation, error) {
	if reservationID <= 0 {
		return postgres.Reservation{}, fmt.Errorf("%w: reservation ID must be positive", domain.ErrInvalidTransaction)
	}
	return s.reservations.Get(ctx, s.store.DB(), reservationID)
}

func (s *Service) settle(ctx context.Context, command SettlementCommand, capture bool) (postgres.Reservation, error) {
	if command.ReservationID <= 0 {
		return postgres.Reservation{}, fmt.Errorf("%w: reservation ID must be positive", domain.ErrInvalidTransaction)
	}
	if err := command.Key.Validate(); err != nil {
		return postgres.Reservation{}, err
	}
	if err := command.Metadata.Validate(); err != nil {
		return postgres.Reservation{}, err
	}
	var updated postgres.Reservation
	err := s.store.WithinTransaction(ctx, func(tx *sql.Tx) error {
		reservation, err := s.reservations.GetForUpdate(ctx, tx, command.ReservationID)
		if err != nil {
			return err
		}
		remaining := reservation.OriginalAmount - reservation.CapturedAmount - reservation.ReleasedAmount
		if remaining <= 0 {
			return domain.ErrReservationOverSettled
		}
		amount := remaining
		if command.Amount != nil {
			amount = *command.Amount
		}
		if err := amount.Validate(); err != nil {
			return err
		}
		if amount > remaining {
			return domain.ErrReservationOverSettled
		}
		owner, err := s.owners.GetForUpdate(ctx, tx, reservation.OwnerID)
		if err != nil {
			return err
		}
		walletCode, _ := WalletAccountCode(reservation.OwnerID)
		reservedAccountCode, _ := ReservedAccountCode(reservation.OwnerID)
		postings := []domain.Posting{{AccountCode: reservedAccountCode, AccountName: "Reserved Wallet", Side: domain.Credit, Amount: amount, Metadata: domain.EmptyMetadata(), EnforcePositive: true}}
		if capture {
			postings = append(postings, domain.Posting{AccountCode: "sink:consumed", AccountName: "sink consumed", Side: domain.Debit, Amount: amount, Metadata: domain.EmptyMetadata()})
		} else {
			postings = append(postings, domain.Posting{AccountCode: walletCode, AccountName: "Wallet", Side: domain.Debit, Amount: amount, Metadata: domain.EmptyMetadata()})
		}
		description := command.Description
		if description == "" {
			if capture {
				description = "reservation capture"
			} else {
				description = "reservation release"
			}
		}
		_, err = s.postInTransaction(ctx, tx, owner, func() domain.TransactionType {
			if capture {
				return domain.Capture
			}
			return domain.Release
		}(), description, command.Key, command.Metadata, &command.ReservationID, postings)
		if err != nil {
			return err
		}
		updated, err = s.reservations.Settle(ctx, tx, reservation, capture, amount)
		return err
	})
	return updated, err
}

// SpendWithOperation ensures externally performed work is paid for only after
// a durable reservation. The callback deliberately runs outside a SQL transaction.
func (s *Service) SpendWithOperation(ctx context.Context, command ReserveCommand, work ExternalWork) (postgres.Reservation, error) {
	if work == nil {
		return postgres.Reservation{}, fmt.Errorf("%w: external work is required", domain.ErrInvalidTransaction)
	}
	reservation, err := s.Reserve(ctx, command)
	if err != nil {
		return reservation, err
	}
	if err = work.Execute(ctx); err == nil {
		amount := reservation.OriginalAmount
		return s.Capture(ctx, SettlementCommand{ReservationID: reservation.ReservationTransactionID, Amount: &amount, Description: command.Description, Metadata: command.Metadata})
	}
	workErr := err
	_, releaseErr := s.Release(ctx, SettlementCommand{ReservationID: reservation.ReservationTransactionID, Description: command.Description, Metadata: command.Metadata})
	if releaseErr != nil {
		return reservation, errors.Join(workErr, releaseErr)
	}
	return reservation, workErr
}

func (s *Service) Adjust(ctx context.Context, command AdjustCommand) (TransactionResult, error) {
	return s.post(ctx, domain.Adjustment, command.OwnerID, command.Description, command.Key, command.Metadata, command.Postings)
}

func (s *Service) post(ctx context.Context, typ domain.TransactionType, ownerID int64, description string, key domain.IdempotencyKey, metadata domain.Metadata, postings []domain.Posting) (result TransactionResult, err error) {
	return s.postWithParent(ctx, typ, ownerID, description, key, metadata, nil, postings, nil)
}
func (s *Service) postWithParent(ctx context.Context, typ domain.TransactionType, ownerID int64, description string, key domain.IdempotencyKey, metadata domain.Metadata, parentID *int64, postings []domain.Posting, after func(*sql.Tx, TransactionResult) error) (result TransactionResult, err error) {
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
	err = s.store.WithinTransaction(ctx, func(tx *sql.Tx) error {
		owner, err := s.owners.GetForUpdate(ctx, tx, ownerID)
		if err != nil {
			return err
		}
		result, err = s.postInTransaction(ctx, tx, owner, typ, description, key, metadata, parentID, postings)
		if err != nil {
			return err
		}
		if after != nil {
			return after(tx, result)
		}
		return nil
	})
	return result, err
}
func (s *Service) postInTransaction(ctx context.Context, tx *sql.Tx, owner domain.Owner, typ domain.TransactionType, description string, key domain.IdempotencyKey, metadata domain.Metadata, parentID *int64, postings []domain.Posting) (result TransactionResult, err error) {
	ownerID := owner.ID
	walletCode, err := WalletAccountCode(ownerID)
	if err != nil {
		return result, err
	}
	ownerRef := domain.OwnerRef{Type: string(owner.Type), ID: owner.ID}
	specs := make([]postgres.AccountSpec, 0, len(postings)+1)
	for _, posting := range postings {
		var accountOwner *int64
		reservedCode, _ := ReservedAccountCode(ownerID)
		if posting.AccountCode == walletCode || posting.AccountCode == reservedCode {
			accountOwner = &ownerID
		}
		if err := ValidateAccountCodeOwnership(posting.AccountCode, accountOwner); err != nil {
			return result, err
		}
		specs = append(specs, postgres.AccountSpec{Code: posting.AccountCode, Name: posting.AccountName, OwnerID: accountOwner, Metadata: posting.Metadata})
	}
	// Always lock the available wallet: settlement workflows do not necessarily
	// post to it, but its projection must not be reset or raced.
	specs = append(specs, postgres.AccountSpec{Code: walletCode, Name: fmt.Sprintf("Owner %d Wallet", ownerID), OwnerID: &ownerID, Metadata: domain.EmptyMetadata()})
	accounts, err := s.ledger.LockExistingAccounts(ctx, tx, specs)
	if err != nil {
		return result, err
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
			return result, domain.ErrInsufficientFunds
		}
	}
	transaction, err := s.ledger.InsertTransaction(ctx, tx, postgres.NewTransaction{Type: typ, Description: description, Owner: ownerRef, ParentTransactionID: parentID, Key: key, Metadata: metadata})
	if err != nil {
		return result, err
	}
	if err := s.ledger.InsertEntries(ctx, tx, transaction.ID, postings, accounts); err != nil {
		return result, err
	}
	for code, account := range accounts {
		if err := s.ledger.UpdateAccountBalance(ctx, tx, account.ID, balances[code]); err != nil {
			return result, err
		}
	}
	if err := s.owners.SetCachedBalance(ctx, tx, ownerID, balances[walletCode]); err != nil {
		return result, err
	}
	result = TransactionResult{TransactionID: transaction.ID, OwnerID: ownerID, AvailableBalance: balances[walletCode]}
	return result, nil
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
		if err != nil {
			return err
		}
		reservedCode, err := ReservedAccountCode(owner.ID)
		if err != nil {
			return err
		}
		_, err = s.ledger.CreateAccount(ctx, tx, postgres.AccountSpec{Code: reservedCode, Name: fmt.Sprintf("Owner %d Reserved Wallet", owner.ID), OwnerID: &owner.ID, Metadata: domain.EmptyMetadata()})
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
