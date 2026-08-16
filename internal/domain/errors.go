package domain

import "errors"

var (
	ErrInvalidAmount          = errors.New("tokenledger: amount must be a positive integer")
	ErrInvalidPosting         = errors.New("tokenledger: invalid posting")
	ErrInvalidMetadata        = errors.New("tokenledger: metadata must be a JSON object")
	ErrImbalancedTransaction  = errors.New("tokenledger: debits must equal credits")
	ErrInvalidTransaction     = errors.New("tokenledger: invalid transaction")
	ErrInvalidIdempotencyKey  = errors.New("tokenledger: external source and external ID must be supplied together")
	ErrInvalidOwner           = errors.New("tokenledger: invalid owner")
	ErrOwnerNotFound          = errors.New("tokenledger: owner not found")
	ErrDuplicateOwner         = errors.New("tokenledger: owner already exists")
	ErrDuplicateTransaction   = errors.New("tokenledger: duplicate transaction")
	ErrInsufficientFunds      = errors.New("tokenledger: insufficient funds")
	ErrTransactionNotFound    = errors.New("tokenledger: transaction not found")
	ErrAccountNotFound        = errors.New("tokenledger: account not found")
	ErrDuplicateAccount       = errors.New("tokenledger: account already exists")
	ErrReservationNotFound    = errors.New("tokenledger: reservation not found")
	ErrReservationOverSettled = errors.New("tokenledger: reservation settlement exceeds original amount")
)
