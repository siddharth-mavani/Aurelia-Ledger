package integration

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"tokenledger/internal/database/postgres"
)

// Integration tests are intentionally opt-in until a PostgreSQL test database is supplied.
// DATABASE_URL must point to an empty, disposable PostgreSQL database.
func TestPostgresConnection(t *testing.T) {
	url := os.Getenv("TOKEN_LEDGER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TOKEN_LEDGER_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	store, err := postgres.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	applySchema(t, store.DB())

	ctx := context.Background()
	t.Run("rejects a header without balanced entries at commit", func(t *testing.T) {
		err := store.WithinTransaction(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO ledger_transactions (transaction_type, description) VALUES ('deposit', 'missing entries')`)
			return err
		})
		if err == nil {
			t.Fatal("expected deferred balance constraint failure")
		}
	})

	var transactionID int64
	err = store.WithinTransaction(ctx, func(tx *sql.Tx) error {
		var walletID, sourceID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO ledger_accounts (code, name) VALUES ('wallet:42', 'User 42 Wallet') RETURNING id`).Scan(&walletID); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `INSERT INTO ledger_accounts (code, name) VALUES ('source:stripe', 'Stripe Token Source') RETURNING id`).Scan(&sourceID); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `INSERT INTO ledger_transactions (transaction_type, description, external_source, external_id) VALUES ('deposit', 'invoice', 'stripe', 'inv-42') RETURNING id`).Scan(&transactionID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_entries (account_id, transaction_id, entry_type, amount) VALUES ($1, $2, 'debit', 100), ($3, $2, 'credit', 100)`, walletID, transactionID, sourceID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("enforces durable journal constraints", func(t *testing.T) {
		if _, err := store.DB().ExecContext(ctx, `UPDATE ledger_transactions SET description = 'changed' WHERE id = $1`, transactionID); err == nil {
			t.Fatal("expected immutable transaction failure")
		}
		if _, err := store.DB().ExecContext(ctx, `INSERT INTO ledger_accounts (code, name) VALUES ('wallet:42', 'duplicate')`); err == nil {
			t.Fatal("expected unique account-code failure")
		}
		if _, err := store.DB().ExecContext(ctx, `INSERT INTO ledger_entries (account_id, transaction_id, entry_type, amount) VALUES (1, $1, 'debit', 0)`, transactionID); err == nil {
			t.Fatal("expected positive amount failure")
		}
	})
}

func applySchema(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	_, _ = db.ExecContext(ctx, `DROP TABLE IF EXISTS schema_migrations, ledger_entries, ledger_transactions, ledger_accounts CASCADE`)
	_, _ = db.ExecContext(ctx, `DROP FUNCTION IF EXISTS ledger_assert_transaction_balanced() CASCADE`)
	_, _ = db.ExecContext(ctx, `DROP FUNCTION IF EXISTS ledger_reject_journal_mutation() CASCADE`)
	migrations, err := postgres.LoadMigrations(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if err := postgres.ApplyMigrations(ctx, db, migrations); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if err := postgres.ApplyMigrations(ctx, db, migrations); err != nil {
		t.Fatalf("rerun migrations: %v", err)
	}
	var applied int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != len(migrations) {
		t.Fatalf("recorded migrations = %d, want %d", applied, len(migrations))
	}
}
