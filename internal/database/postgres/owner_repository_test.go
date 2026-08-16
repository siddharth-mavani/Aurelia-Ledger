package postgres_test

import (
	"errors"
	"testing"

	"aurelialedger/internal/domain"
)

func TestOwnerValidation(t *testing.T) {
	owner := domain.Owner{Type: domain.CustomerOwner, ExternalRef: "alice", DisplayName: "Alice", Metadata: domain.EmptyMetadata()}
	if err := owner.Validate(); err != nil {
		t.Fatal(err)
	}
	owner.Type = "other"
	if err := owner.Validate(); !errors.Is(err, domain.ErrInvalidOwner) {
		t.Fatalf("got %v", err)
	}
}
