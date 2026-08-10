package domain

import "errors"

var (
	ErrInvalidAmount         = errors.New("tokenledger: amount must be a positive integer")
	ErrInvalidPosting        = errors.New("tokenledger: invalid posting")
	ErrInvalidMetadata       = errors.New("tokenledger: metadata must be a JSON object")
	ErrImbalancedTransaction = errors.New("tokenledger: debits must equal credits")
	ErrInvalidTransaction    = errors.New("tokenledger: invalid transaction")
	ErrInvalidIdempotencyKey = errors.New("tokenledger: external source and external ID must be supplied together")
)
