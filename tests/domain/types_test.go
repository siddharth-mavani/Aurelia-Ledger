package domain_test

import (
	"encoding/json"
	"errors"
	"testing"

	"aurelialedger/internal/domain"
)

func validPostings() []domain.Posting {
	return []domain.Posting{
		{AccountCode: "wallet:42", AccountName: "User 42 Wallet", Side: domain.Debit, Amount: 100, Metadata: domain.EmptyMetadata()},
		{AccountCode: "source:stripe", AccountName: "Stripe Token Source", Side: domain.Credit, Amount: 100, Metadata: domain.EmptyMetadata()},
	}
}

func TestValidatePostings(t *testing.T) {
	t.Run("accepts balanced postings", func(t *testing.T) {
		if err := domain.ValidatePostings(validPostings()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("rejects empty postings", func(t *testing.T) {
		if err := domain.ValidatePostings(nil); !errors.Is(err, domain.ErrImbalancedTransaction) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("rejects imbalance", func(t *testing.T) {
		postings := validPostings()
		postings[1].Amount = 99
		if err := domain.ValidatePostings(postings); !errors.Is(err, domain.ErrImbalancedTransaction) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("rejects invalid side and amount", func(t *testing.T) {
		postings := validPostings()
		postings[0].Side = "other"
		if err := domain.ValidatePostings(postings); !errors.Is(err, domain.ErrInvalidPosting) {
			t.Fatalf("got %v", err)
		}
		postings = validPostings()
		postings[0].Amount = 0
		if err := domain.ValidatePostings(postings); !errors.Is(err, domain.ErrInvalidAmount) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestMetadataAndIdempotencyValidation(t *testing.T) {
	if err := domain.Metadata(`{"plan":"pro"}`).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := domain.Metadata(`[]`).Validate(); !errors.Is(err, domain.ErrInvalidMetadata) {
		t.Fatalf("got %v", err)
	}
	if err := (domain.IdempotencyKey{Source: "stripe"}).Validate(); !errors.Is(err, domain.ErrInvalidIdempotencyKey) {
		t.Fatalf("got %v", err)
	}
	if err := (domain.IdempotencyKey{Source: "stripe", ID: "invoice-1"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataJSONRoundTrip(t *testing.T) {
	var metadata domain.Metadata
	if err := json.Unmarshal([]byte(`{"origin":"api"}`), &metadata); err != nil {
		t.Fatal(err)
	}
	if err := metadata.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"origin":"api"}` {
		t.Fatalf("metadata JSON = %s", encoded)
	}
}
