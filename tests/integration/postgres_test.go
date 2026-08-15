package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"tokenledger/internal/database/postgres"
	"tokenledger/internal/domain"
	"tokenledger/internal/httpapi"
	"tokenledger/internal/ledger"
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

	t.Run("executes owner posting workflows", func(t *testing.T) {
		service := ledger.NewService(store)
		if _, err := service.RegisterSource(ctx, ledger.RegisterSourceCommand{Name: "manual", DisplayName: "Manual Source", Metadata: domain.EmptyMetadata()}); err != nil {
			t.Fatal(err)
		}
		owner, err := service.CreateOwner(ctx, ledger.CreateOwnerCommand{
			Type: domain.CustomerOwner, ExternalRef: "alice@example.com", DisplayName: "Alice", Metadata: domain.EmptyMetadata(),
		})
		if err != nil {
			t.Fatal(err)
		}
		var walletOwnerID int64
		if err := store.DB().QueryRowContext(ctx, `SELECT owner_id FROM ledger_accounts WHERE code = $1`, "wallet:"+strconv.FormatInt(owner.ID, 10)).Scan(&walletOwnerID); err != nil || walletOwnerID != owner.ID {
			t.Fatalf("owner wallet = %d, %v", walletOwnerID, err)
		}
		if _, err := store.DB().ExecContext(ctx, `INSERT INTO ledger_transactions (transaction_type, description, owner_type, owner_id) VALUES ('deposit', 'wrong owner type', 'team', $1)`, owner.ID); err == nil {
			t.Fatal("expected composite owner foreign key failure")
		}
		if _, err := service.Deposit(ctx, ledger.DepositCommand{OwnerID: owner.ID, Amount: 100, Description: "unregistered", Key: domain.IdempotencyKey{Source: "unknown", ID: "invoice-0"}, Metadata: domain.EmptyMetadata()}); !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatalf("unregistered source = %v", err)
		}
		deposit, err := service.Deposit(ctx, ledger.DepositCommand{OwnerID: owner.ID, Amount: 100, Description: "purchase", Key: domain.IdempotencyKey{Source: "manual", ID: "invoice-1"}, Metadata: domain.EmptyMetadata()})
		if err != nil || deposit.AvailableBalance != 100 {
			t.Fatalf("deposit %#v, %v", deposit, err)
		}
		if _, err := service.Deposit(ctx, ledger.DepositCommand{OwnerID: owner.ID, Amount: 100, Description: "retry", Key: domain.IdempotencyKey{Source: "manual", ID: "invoice-1"}, Metadata: domain.EmptyMetadata()}); !errors.Is(err, domain.ErrDuplicateTransaction) {
			t.Fatalf("duplicate = %v", err)
		}
		spend, err := service.Spend(ctx, ledger.SpendCommand{OwnerID: owner.ID, Amount: 40, Description: "use", Key: domain.IdempotencyKey{Source: "app", ID: "use-1"}, Metadata: domain.EmptyMetadata()})
		if err != nil || spend.AvailableBalance != 60 {
			t.Fatalf("spend %#v, %v", spend, err)
		}
		if _, err := service.Spend(ctx, ledger.SpendCommand{OwnerID: owner.ID, Amount: 61, Description: "overspend", Metadata: domain.EmptyMetadata()}); !errors.Is(err, domain.ErrInsufficientFunds) {
			t.Fatalf("overspend = %v", err)
		}
		balance, err := service.GetBalance(ctx, owner.ID)
		if err != nil || balance != 60 {
			t.Fatalf("balance %d, %v", balance, err)
		}
		transaction, err := service.GetTransaction(ctx, spend.TransactionID)
		if err != nil || len(transaction.Entries) != 2 {
			t.Fatalf("transaction %#v, %v", transaction, err)
		}
		page, err := service.ListTransactions(ctx, owner.ID, 1, nil)
		if err != nil || len(page.Items) != 1 || page.Next == nil {
			t.Fatalf("page %#v, %v", page, err)
		}
		if _, err := store.DB().ExecContext(ctx, `UPDATE ledger_owners SET cached_balance = 1 WHERE id = $1`, owner.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().ExecContext(ctx, `UPDATE ledger_accounts SET current_balance = 1 WHERE code = $1`, "wallet:"+strconv.FormatInt(owner.ID, 10)); err != nil {
			t.Fatal(err)
		}
		previous, repairedBalance, repaired, err := service.ReconcileOwner(ctx, owner.ID)
		if err != nil || previous != 1 || repairedBalance != 60 || !repaired {
			t.Fatalf("reconcile %d %d %t %v", previous, repairedBalance, repaired, err)
		}
	})

	t.Run("serializes concurrent owner and posting workflows", func(t *testing.T) {
		service := ledger.NewService(store)
		if _, err := service.RegisterSource(ctx, ledger.RegisterSourceCommand{Name: "test", DisplayName: "Test Source", Metadata: domain.EmptyMetadata()}); err != nil {
			t.Fatal(err)
		}
		const workers = 5
		owners := make(chan domain.Owner, workers)
		errs := make(chan error, workers)
		var group sync.WaitGroup
		for range workers {
			group.Add(1)
			go func() {
				defer group.Done()
				owner, err := service.CreateOwner(ctx, ledger.CreateOwnerCommand{Type: domain.TeamOwner, ExternalRef: "concurrent-team", DisplayName: "Concurrent", Metadata: domain.EmptyMetadata()})
				if err == nil {
					owners <- owner
				}
				errs <- err
			}()
		}
		group.Wait()
		close(owners)
		close(errs)
		var owner domain.Owner
		created := 0
		for err := range errs {
			if err == nil {
				created++
			} else if !errors.Is(err, domain.ErrDuplicateOwner) {
				t.Fatalf("create error: %v", err)
			}
		}
		for candidate := range owners {
			owner = candidate
		}
		if created != 1 {
			t.Fatalf("created owners = %d, want 1", created)
		}

		errs = make(chan error, workers)
		group = sync.WaitGroup{}
		for i := range workers {
			group.Add(1)
			go func(i int) {
				defer group.Done()
				_, err := service.Deposit(ctx, ledger.DepositCommand{OwnerID: owner.ID, Amount: 10, Description: "concurrent deposit", Key: domain.IdempotencyKey{Source: "test", ID: fmt.Sprintf("deposit-%d", i)}, Metadata: domain.EmptyMetadata()})
				errs <- err
			}(i)
		}
		group.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("deposit error: %v", err)
			}
		}
		balance, err := service.GetBalance(ctx, owner.ID)
		if err != nil || balance != 50 {
			t.Fatalf("deposited balance %d, %v", balance, err)
		}

		errs = make(chan error, 2)
		group = sync.WaitGroup{}
		for i := range 2 {
			group.Add(1)
			go func(i int) {
				defer group.Done()
				_, err := service.Spend(ctx, ledger.SpendCommand{OwnerID: owner.ID, Amount: 30, Description: "concurrent spend", Key: domain.IdempotencyKey{Source: "test", ID: fmt.Sprintf("spend-%d", i)}, Metadata: domain.EmptyMetadata()})
				errs <- err
			}(i)
		}
		group.Wait()
		close(errs)
		successes := 0
		for err := range errs {
			if err == nil {
				successes++
			} else if !errors.Is(err, domain.ErrInsufficientFunds) {
				t.Fatalf("spend error: %v", err)
			}
		}
		if successes != 1 {
			t.Fatalf("successful spends = %d, want 1", successes)
		}
		balance, err = service.GetBalance(ctx, owner.ID)
		if err != nil || balance != 20 {
			t.Fatalf("spent balance %d, %v", balance, err)
		}

		errs = make(chan error, 2)
		group = sync.WaitGroup{}
		for i := range 2 {
			group.Add(1)
			go func(i int) {
				defer group.Done()
				postings := []domain.Posting{{AccountCode: fmt.Sprintf("wallet:%d", owner.ID), AccountName: "Concurrent Wallet", Side: domain.Debit, Amount: 1, Metadata: domain.EmptyMetadata()}, {AccountCode: "source:test", AccountName: "Test Source", Side: domain.Credit, Amount: 1, Metadata: domain.EmptyMetadata()}}
				if i == 1 {
					postings[0], postings[1] = postings[1], postings[0]
				}
				_, err := service.Adjust(ctx, ledger.AdjustCommand{OwnerID: owner.ID, Description: "opposite order", Key: domain.IdempotencyKey{Source: "test", ID: fmt.Sprintf("adjust-%d", i)}, Metadata: domain.EmptyMetadata(), Postings: postings})
				errs <- err
			}(i)
		}
		group.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("adjustment error: %v", err)
			}
		}
		balance, err = service.GetBalance(ctx, owner.ID)
		if err != nil || balance != 22 {
			t.Fatalf("adjusted balance %d, %v", balance, err)
		}
	})

	t.Run("serves every Phase 2 endpoint", func(t *testing.T) {
		api := httpapi.New(ledger.NewService(store), "test-token", store.DB().PingContext)
		request := func(method, path, body string, authorized bool) *httptest.ResponseRecorder {
			recorder := httptest.NewRecorder()
			httpRequest := httptest.NewRequest(method, path, bytes.NewBufferString(body))
			if body != "" {
				httpRequest.Header.Set("Content-Type", "application/json")
			}
			if authorized {
				httpRequest.Header.Set("Authorization", "Bearer test-token")
			}
			api.ServeHTTP(recorder, httpRequest)
			return recorder
		}
		if response := request(http.MethodGet, "/v1/owners/1/balance", "", false); response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorized = %d", response.Code)
		}
		created := request(http.MethodPost, "/v1/owners", `{"type":"project","external_ref":"api-project","display_name":"API Project","metadata":{"origin":"test"}}`, true)
		if created.Code != http.StatusCreated {
			t.Fatalf("create owner = %d: %s", created.Code, created.Body.String())
		}
		var owner domain.Owner
		if err := json.NewDecoder(created.Body).Decode(&owner); err != nil {
			t.Fatal(err)
		}
		if response := request(http.MethodPost, fmt.Sprintf("/v1/owners/%d/deposits", owner.ID), `{"amount":100,"description":"unregistered","external_source":"api-source","external_id":"missing","metadata":{}}`, true); response.Code != http.StatusNotFound {
			t.Fatalf("unregistered source = %d: %s", response.Code, response.Body.String())
		}
		registered := request(http.MethodPost, "/v1/register-sources", `{"name":"api-source","display_name":"API Source","metadata":{}}`, true)
		if registered.Code != http.StatusCreated {
			t.Fatalf("register source = %d: %s", registered.Code, registered.Body.String())
		}
		deposit := request(http.MethodPost, fmt.Sprintf("/v1/owners/%d/deposits", owner.ID), `{"amount":100,"description":"API deposit","external_source":"api-source","external_id":"api-invoice","metadata":{"invoice":true}}`, true)
		if deposit.Code != http.StatusCreated {
			t.Fatalf("deposit = %d: %s", deposit.Code, deposit.Body.String())
		}
		var posted ledger.TransactionResult
		if err := json.NewDecoder(deposit.Body).Decode(&posted); err != nil {
			t.Fatal(err)
		}
		if response := request(http.MethodPost, fmt.Sprintf("/v1/owners/%d/deposits", owner.ID), `{"amount":100,"description":"retry","external_source":"api-source","external_id":"api-invoice","metadata":{}}`, true); response.Code != http.StatusConflict {
			t.Fatalf("duplicate = %d: %s", response.Code, response.Body.String())
		}
		if response := request(http.MethodPost, fmt.Sprintf("/v1/owners/%d/spends", owner.ID), `{"amount":101,"description":"too much","metadata":{}}`, true); response.Code != http.StatusConflict {
			t.Fatalf("overspend = %d: %s", response.Code, response.Body.String())
		}
		if response := request(http.MethodPost, fmt.Sprintf("/v1/owners/%d/spends", owner.ID), `{"amount":25,"description":"API spend","external_source":"app","external_id":"api-spend","metadata":{}}`, true); response.Code != http.StatusCreated {
			t.Fatalf("spend = %d: %s", response.Code, response.Body.String())
		}
		adjustment := fmt.Sprintf(`{"description":"API adjustment","metadata":{},"postings":[{"account_code":"wallet:%d","account_name":"API wallet","side":"debit","amount":5,"metadata":{}},{"account_code":"source:api-source","account_name":"API source","side":"credit","amount":5,"metadata":{}}]}`, owner.ID)
		if response := request(http.MethodPost, fmt.Sprintf("/v1/owners/%d/adjustments", owner.ID), adjustment, true); response.Code != http.StatusCreated {
			t.Fatalf("adjustment = %d: %s", response.Code, response.Body.String())
		}
		if response := request(http.MethodGet, fmt.Sprintf("/v1/owners/%d/balance", owner.ID), "", true); response.Code != http.StatusOK {
			t.Fatalf("balance = %d: %s", response.Code, response.Body.String())
		}
		history := request(http.MethodGet, fmt.Sprintf("/v1/owners/%d/transactions?limit=1", owner.ID), "", true)
		if history.Code != http.StatusOK {
			t.Fatalf("history = %d: %s", history.Code, history.Body.String())
		}
		var list struct {
			Items      []domain.Transaction `json:"items"`
			NextCursor string               `json:"next_cursor"`
		}
		if err := json.NewDecoder(history.Body).Decode(&list); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) != 1 || list.NextCursor == "" {
			t.Fatalf("history %#v", list)
		}
		if response := request(http.MethodGet, fmt.Sprintf("/v1/owners/%d/transactions?cursor=%s", owner.ID, list.NextCursor), "", true); response.Code != http.StatusOK {
			t.Fatalf("cursor history = %d: %s", response.Code, response.Body.String())
		}
		if response := request(http.MethodGet, fmt.Sprintf("/v1/owners/%d/transactions?cursor=bad", owner.ID), "", true); response.Code != http.StatusBadRequest {
			t.Fatalf("bad cursor = %d", response.Code)
		}
		if response := request(http.MethodGet, fmt.Sprintf("/v1/transactions/%d", posted.TransactionID), "", true); response.Code != http.StatusOK {
			t.Fatalf("transaction = %d: %s", response.Code, response.Body.String())
		}
		if response := request(http.MethodPost, fmt.Sprintf("/v1/owners/%d/reconcile", owner.ID), "", true); response.Code != http.StatusOK {
			t.Fatalf("reconcile = %d: %s", response.Code, response.Body.String())
		}
	})
}

func applySchema(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	_, _ = db.ExecContext(ctx, `DROP TABLE IF EXISTS schema_migrations, ledger_entries, ledger_transactions, ledger_accounts, ledger_owners CASCADE`)
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
