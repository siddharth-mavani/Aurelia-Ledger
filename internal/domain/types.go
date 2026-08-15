package domain

import (
	"encoding/json"
	"fmt"
	"strings"
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

// UnmarshalJSON preserves a JSON value as raw bytes so handlers can validate
// that it is an object before it reaches the database.
func (m *Metadata) UnmarshalJSON(data []byte) error {
	*m = append((*m)[:0], data...)
	return nil
}

// MarshalJSON writes metadata as its JSON object rather than encoding its raw
// bytes as a base64 string.
func (m Metadata) MarshalJSON() ([]byte, error) {
	if len(m) == 0 {
		return []byte(`{}`), nil
	}
	return m, nil
}

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
	Type string `json:"type"`
	ID   int64  `json:"id"`
}

func (o OwnerRef) Valid() bool { return o.Type != "" && o.ID > 0 }

type OwnerType string

const (
	CustomerOwner OwnerType = "customer"
	TeamOwner     OwnerType = "team"
	ProjectOwner  OwnerType = "project"
)

func (t OwnerType) Validate() error {
	switch t {
	case CustomerOwner, TeamOwner, ProjectOwner:
		return nil
	default:
		return fmt.Errorf("%w: owner type %q", ErrInvalidOwner, t)
	}
}

type Owner struct {
	ID            int64     `json:"id"`
	Type          OwnerType `json:"type"`
	ExternalRef   string    `json:"external_ref"`
	DisplayName   string    `json:"display_name"`
	CachedBalance int64     `json:"cached_balance"`
	Metadata      Metadata  `json:"metadata"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (o Owner) Validate() error {
	if err := o.Type.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(o.ExternalRef) == "" || strings.TrimSpace(o.DisplayName) == "" {
		return fmt.Errorf("%w: external reference and display name are required", ErrInvalidOwner)
	}
	return o.Metadata.Validate()
}

// IdempotencyKey identifies a request in an external source namespace.
type IdempotencyKey struct {
	Source string `json:"source"`
	ID     string `json:"id"`
}

func (k IdempotencyKey) Validate() error {
	if (k.Source == "") != (k.ID == "") {
		return ErrInvalidIdempotencyKey
	}
	return nil
}

type Account struct {
	ID             int64     `json:"id"`
	Code           string    `json:"code"`
	Name           string    `json:"name"`
	CurrentBalance int64     `json:"current_balance"`
	Metadata       Metadata  `json:"metadata"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type Transaction struct {
	ID                  int64           `json:"id"`
	Type                TransactionType `json:"type"`
	Description         string          `json:"description"`
	Owner               *OwnerRef       `json:"owner"`
	ParentTransactionID *int64          `json:"parent_transaction_id,omitempty"`
	IdempotencyKey      IdempotencyKey  `json:"idempotency_key"`
	Metadata            Metadata        `json:"metadata"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

type Entry struct {
	ID            int64     `json:"id"`
	AccountID     int64     `json:"account_id"`
	TransactionID int64     `json:"transaction_id"`
	Side          EntrySide `json:"side"`
	Amount        Amount    `json:"amount"`
	Metadata      Metadata  `json:"metadata"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Posting is an unpersisted balanced entry supplied to a write operation.
type Posting struct {
	AccountCode     string    `json:"account_code"`
	AccountName     string    `json:"account_name"`
	Side            EntrySide `json:"side"`
	Amount          Amount    `json:"amount"`
	Metadata        Metadata  `json:"metadata"`
	EnforcePositive bool      `json:"-"`
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
