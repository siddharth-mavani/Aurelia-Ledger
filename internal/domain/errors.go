package domain

import "errors"

var (
	ErrInvalidAmount          = errors.New("aurelia-ledger: amount must be a positive integer")
	ErrInvalidPosting         = errors.New("aurelia-ledger: invalid posting")
	ErrInvalidMetadata        = errors.New("aurelia-ledger: metadata must be a JSON object")
	ErrImbalancedTransaction  = errors.New("aurelia-ledger: debits must equal credits")
	ErrInvalidTransaction     = errors.New("aurelia-ledger: invalid transaction")
	ErrInvalidIdempotencyKey  = errors.New("aurelia-ledger: external source and external ID must be supplied together")
	ErrInvalidOwner           = errors.New("aurelia-ledger: invalid owner")
	ErrOwnerNotFound          = errors.New("aurelia-ledger: owner not found")
	ErrDuplicateOwner         = errors.New("aurelia-ledger: owner already exists")
	ErrDuplicateTransaction   = errors.New("aurelia-ledger: duplicate transaction")
	ErrInsufficientFunds      = errors.New("aurelia-ledger: insufficient funds")
	ErrTransactionNotFound    = errors.New("aurelia-ledger: transaction not found")
	ErrAccountNotFound        = errors.New("aurelia-ledger: account not found")
	ErrDuplicateAccount       = errors.New("aurelia-ledger: account already exists")
	ErrReservationNotFound    = errors.New("aurelia-ledger: reservation not found")
	ErrReservationOverSettled = errors.New("aurelia-ledger: reservation settlement exceeds original amount")
)
