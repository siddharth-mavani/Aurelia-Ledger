package ledger

import (
	"errors"
	"testing"

	"aurelialedger/internal/domain"
)

func TestValidateAccountCodeOwnership(t *testing.T) {
	ownerID := int64(42)
	for _, test := range []struct {
		name    string
		code    string
		ownerID *int64
		wantErr bool
	}{
		{name: "owner wallet", code: "wallet:42", ownerID: &ownerID},
		{name: "owner reserved wallet", code: "wallet:42:reserved", ownerID: &ownerID},
		{name: "wrong reserved owner", code: "wallet:99:reserved", ownerID: &ownerID, wantErr: true},
		{name: "invalid wallet suffix", code: "wallet:42:other", ownerID: &ownerID, wantErr: true},
		{name: "system source", code: "source:stripe"},
		{name: "system sink", code: "sink:spend"},
		{name: "wrong wallet owner", code: "wallet:99", ownerID: &ownerID, wantErr: true},
		{name: "wallet without owner", code: "wallet:42", wantErr: true},
		{name: "owned source", code: "source:stripe", ownerID: &ownerID, wantErr: true},
		{name: "unnamed sink", code: "sink:", wantErr: true},
		{name: "unknown prefix", code: "cash:main", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateAccountCodeOwnership(test.code, test.ownerID)
			if test.wantErr && !errors.Is(err, domain.ErrInvalidPosting) {
				t.Fatalf("got %v", err)
			}
			if !test.wantErr && err != nil {
				t.Fatal(err)
			}
		})
	}
}
