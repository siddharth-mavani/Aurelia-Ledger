package ledger

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"tokenledger/internal/domain"
)

var sourceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

const (
	walletPrefix = "wallet:"
	sourcePrefix = "source:"
	sinkPrefix   = "sink:"
)

// WalletAccountCode is the only account code permitted for an owner's wallet.
func WalletAccountCode(ownerID int64) (string, error) {
	if ownerID <= 0 {
		return "", fmt.Errorf("%w: wallet owner ID must be positive", domain.ErrInvalidOwner)
	}
	return walletPrefix + strconv.FormatInt(ownerID, 10), nil
}

// ReservedAccountCode is the owner-scoped account holding funds unavailable for spend.
func ReservedAccountCode(ownerID int64) (string, error) {
	walletCode, err := WalletAccountCode(ownerID)
	if err != nil {
		return "", err
	}
	return walletCode + ":reserved", nil
}

// SourceAccountCode returns the canonical code for an explicitly registered source.
func SourceAccountCode(name string) (string, error) {
	if !sourceNamePattern.MatchString(name) {
		return "", fmt.Errorf("%w: source name must use lowercase letters, digits, hyphens, or underscores", domain.ErrInvalidPosting)
	}
	return sourcePrefix + name, nil
}

// ValidateAccountCodeOwnership enforces the Phase 2 account-code convention.
// Wallets are owner-scoped; sources and sinks are system accounts and therefore
// must not be associated with an owner.
func ValidateAccountCodeOwnership(code string, ownerID *int64) error {
	switch {
	case strings.HasPrefix(code, walletPrefix):
		if ownerID == nil || *ownerID <= 0 {
			return fmt.Errorf("%w: wallet account requires an owner", domain.ErrInvalidPosting)
		}
		walletCode, err := WalletAccountCode(*ownerID)
		if err != nil {
			return err
		}
		reservedCode, err := ReservedAccountCode(*ownerID)
		if err != nil || (code != walletCode && code != reservedCode) {
			return fmt.Errorf("%w: wallet code %q does not match owner %d", domain.ErrInvalidPosting, code, *ownerID)
		}
		return nil
	case strings.HasPrefix(code, sourcePrefix), strings.HasPrefix(code, sinkPrefix):
		if strings.TrimPrefix(strings.TrimPrefix(code, sourcePrefix), sinkPrefix) == "" {
			return fmt.Errorf("%w: system account code requires a name", domain.ErrInvalidPosting)
		}
		if ownerID != nil {
			return fmt.Errorf("%w: system account %q cannot have an owner", domain.ErrInvalidPosting, code)
		}
		return nil
	default:
		return fmt.Errorf("%w: account code %q must start with wallet:, source:, or sink:", domain.ErrInvalidPosting, code)
	}
}
