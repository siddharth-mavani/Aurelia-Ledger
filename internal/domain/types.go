package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// Amount is a token quantity. Tokens are stored in indivisible base units.
type Amount int64

func (a Amount) Validate() error {
	if a <= 0 {
		return ErrInvalidAmount
	}
	return nil
}

// Metadata is an optional JSON object attached to a ledger record.
type Metadata json.RawMessage

func EmptyMetadata() Metadata { return Metadata(`{}`) }

func (m Metadata) Validate() error {
	if len(m) == 0 {
		return nil
	}
	var value map[string]any
	if err := json.Unmarshal(m, &value); err != nil || value == nil {
		return ErrInvalidMetadata
	}
	return nil
}

type EntrySide string

const (
	Debit  EntrySide = "debit"
	Credit EntrySide = "credit"
)

func (s EntrySide) Validate() error {
	if s != Debit && s != Credit {
		return fmt.Errorf("%w: entry side %q", ErrInvalidPosting, s)
	}
	return nil
}

type TransactionType string

const (
	Deposit    TransactionType = "deposit"
	Spend      TransactionType = "spend"
	Reserve    TransactionType = "reserve"
	Capture    TransactionType = "capture"
	Release    TransactionType = "release"
	Adjustment TransactionType = "adjustment"
)

func (t TransactionType) Validate() error {
	switch t {
	case Deposit, Spend, Reserve, Capture, Release, Adjustment:
		return nil
	default:
		return fmt.Errorf("%w: transaction type %q", ErrInvalidTransaction, t)
	}
}

// OwnerRef identifies the application record that owns a transaction.
type OwnerRef struct {
	Type string
	ID   int64
}

func (o OwnerRef) Valid() bool { return o.Type != "" && o.ID > 0 }

// IdempotencyKey identifies a request in an external source namespace.
type IdempotencyKey struct {
	Source string
	ID     string
}

func (k IdempotencyKey) Validate() error {
	if (k.Source == "") != (k.ID == "") {
		return ErrInvalidIdempotencyKey
	}
	return nil
}

type Account struct {
	ID             int64
	Code           string
	Name           string
	CurrentBalance int64
	Metadata       Metadata
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Transaction struct {
	ID                  int64
	Type                TransactionType
	Description         string
	Owner               *OwnerRef
	ParentTransactionID *int64
	IdempotencyKey      IdempotencyKey
	Metadata            Metadata
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type Entry struct {
	ID            int64
	AccountID     int64
	TransactionID int64
	Side          EntrySide
	Amount        Amount
	Metadata      Metadata
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Posting is an unpersisted balanced entry supplied to a write operation.
type Posting struct {
	AccountCode     string
	AccountName     string
	Side            EntrySide
	Amount          Amount
	Metadata        Metadata
	EnforcePositive bool
}

func (p Posting) Validate() error {
	if p.AccountCode == "" || p.AccountName == "" {
		return fmt.Errorf("%w: account code and name are required", ErrInvalidPosting)
	}
	if err := p.Side.Validate(); err != nil {
		return err
	}
	if err := p.Amount.Validate(); err != nil {
		return err
	}
	return p.Metadata.Validate()
}

// ValidatePostings accepts only non-empty, balanced debit/credit posting sets.
func ValidatePostings(postings []Posting) error {
	if len(postings) == 0 {
		return fmt.Errorf("%w: transaction must have entries", ErrImbalancedTransaction)
	}
	var debits, credits int64
	for _, posting := range postings {
		if err := posting.Validate(); err != nil {
			return err
		}
		if posting.Side == Debit {
			debits += int64(posting.Amount)
		} else {
			credits += int64(posting.Amount)
		}
	}
	if debits != credits {
		return fmt.Errorf("%w: debits (%d) != credits (%d)", ErrImbalancedTransaction, debits, credits)
	}
	return nil
}
