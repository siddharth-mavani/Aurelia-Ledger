# Aurelia Ledger application implementation plan

## Architecture and conventions

Aurelia Ledger is a locally runnable HTTP application backed by PostgreSQL. The server starts in `cmd/server`; domain rules live in `internal/domain`; database access and SQL transaction boundaries live in `internal/database/postgres`; HTTP code belongs in `internal/httpapi`; application workflows belong in `internal/ledger`.

All tests live below `tests/`: deterministic unit tests in `tests/domain` and disposable-PostgreSQL API/database tests in `tests/integration`. PostgreSQL is the only runtime database. Amounts are positive `int64` token base units. Account balance is `sum(debits) - sum(credits)`. Journal headers and entries are append-only; account/owner balances are mutable projections updated in the same transaction as their postings.

## Phase 1 — Application and durable journal foundation

**Status:** completed.

### Implemented components

- `cmd/server/main.go`: process entrypoint, structured logging, PostgreSQL startup check, graceful SIGINT/SIGTERM shutdown, and `GET /healthz`.
- `internal/config`: reads mandatory `DATABASE_URL` and optional `LISTEN_ADDR` (default `:8080`), without logging credentials.
- `internal/domain`: `Amount`, JSON-object `Metadata`, debit/credit `EntrySide`, `TransactionType`, `OwnerRef`, `IdempotencyKey`, persisted records, posting validation, and typed errors.
- `internal/database/postgres`: `pgx` database connection and `database/sql` transaction helper.
- `migrations/000001_create_ledger.sql`: accounts, transaction headers, entries, indexes, JSON/type/amount constraints, immutable journal triggers, and deferred commit-time balance checks.
- `cmd/migrate` and `internal/database/postgres/migrator.go`: lexicographically ordered, advisory-lock-protected migration application with atomic migration records.

### Validation and local execution

```bash
cd /Users/smavani/dev/Aurelia-Ledger
GOCACHE=/private/tmp/aurelia_ledger_gocache go test ./...
GOCACHE=/private/tmp/aurelia_ledger_gocache go test -race ./...
GOCACHE=/private/tmp/aurelia_ledger_gocache go vet ./...

DATABASE_URL="$DATABASE_URL" go run ./cmd/migrate up
DATABASE_URL="$DATABASE_URL" go run ./cmd/server
curl http://localhost:8080/healthz
```

Set `AURELIA_LEDGER_TEST_DATABASE_URL` to a disposable PostgreSQL database to execute the opt-in schema integration tests.

### Migration policy

**Status:** completed. `cmd/migrate up` creates `schema_migrations(version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`, takes a PostgreSQL advisory lock, applies lexicographically ordered `migrations/*.sql` files not recorded in that table, and records each successful file in the same transaction before releasing the lock. The server never applies migrations automatically. `cmd/migrate up` is required before server startup; rerunning it is idempotent.

## Phase 2 — Owner model, posting service, and HTTP API

**Objective:** make the application create owners and execute deposit, spend, adjustment, history, balance, and reconciliation workflows.

### Database work

- Add `migrations/000002_create_owners.sql` with `ledger_owners`: identity ID, owner type, unique external reference within type, display name, cached available balance, JSON metadata, and timestamps.
- Add `ledger_accounts.owner_id` and owner-account indexes. Wallet accounts use one owner; source and sink accounts remain system-owned. Retain `owner_type` on transaction headers so the model can support customer, team, and project ownership.
- Add foreign-key/index migrations only in forward-compatible steps; do not modify the Phase-1 migration after it has been applied anywhere.

`ledger_owners.owner_type` is one of `customer`, `team`, or `project`; `external_ref` and `display_name` are non-empty. `ledger_transactions.owner_id` becomes a nullable foreign key to `ledger_owners(id)`: it identifies the business owner of a transaction. `owner_type` remains a non-null value whenever `owner_id` is present; the posting service verifies it equals the referenced owner's type. `ledger_accounts.owner_id` is nullable and references `ledger_owners(id)`: it identifies the owner of a wallet account. `ledger_entries.account_id` continues to reference `ledger_accounts(id)` and identifies the account affected by that entry. No `account_role` column is used; account code is the canonical account identity and the posting service enforces `wallet:<owner_id>` with matching `ledger_accounts.owner_id`, plus system-owned `source:<name>` and `sink:<name>` accounts with `owner_id IS NULL`.

### Service and database implementation

- Add `internal/database/postgres/owner_repository.go` and `ledger_repository.go`. All queries use parameters, `context.Context`, and a caller-provided `*sql.Tx` for writes.
- Add `internal/ledger/service.go` with typed commands/results: `CreateOwner`, `Deposit`, `Spend`, `Adjust`, `GetBalance`, `ListTransactions`, `GetTransaction`, `CalculateBalance`, and `ReconcileOwner`.
- Add `internal/ledger/posting.go` as the sole journal-write path. It validates a command and `ValidatePostings`, starts one transaction, creates/fetches accounts, locks rows by ascending account code using `SELECT ... FOR UPDATE`, applies cache deltas, inserts header/entries, updates the owner available balance, and commits.
- `OwnerRef` on every owner-scoped transaction links the header to its owner for history queries, wallet lookup, authorization, and cached balance updates.
- Use `EnforcePositive=true` for wallet credits in spend operations. Keep it false in adjustments so explicit corrective/reversal postings can create a negative account balance when necessary.

### HTTP API

Add `internal/httpapi` for routing, JSON decoding, request IDs, validation, and stable JSON errors. Require `Authorization: Bearer <token>` for `/v1/*`; load the local operator token only from `AURELIA_LEDGER_API_TOKEN` and never log it.

```text
POST /v1/owners
POST /v1/register-sources
POST /v1/owners/{ownerID}/deposits
POST /v1/owners/{ownerID}/spends
POST /v1/owners/{ownerID}/adjustments
GET  /v1/owners/{ownerID}/balance
GET  /v1/owners/{ownerID}/transactions
GET  /v1/transactions/{transactionID}
POST /v1/owners/{ownerID}/reconcile
```

Return `validation_error` (400), `unauthorized` (401), `not_found` (404), `insufficient_funds` (409), `duplicate_transaction` (409), or `internal_error` (500).

Every successful response is JSON. Every error has this exact shape:

```json
{"error":{"code":"validation_error","message":"amount must be positive"}}
```

`POST /v1/owners` accepts `{"type":"customer","external_ref":"alice@example.com","display_name":"Alice","metadata":{}}` and returns `201` with `{"id":1,"type":"customer","external_ref":"alice@example.com","display_name":"Alice","cached_balance":0,"metadata":{}}`.

`POST /v1/register-sources` accepts `{"name":"stripe","display_name":"Stripe","metadata":{}}` and creates the system-owned `source:stripe` account. Source names use lowercase letters, digits, hyphens, and underscores. Deposits reject unregistered sources; `source:other` and `sink:spend` are provisioned by migration. Owner creation creates `wallet:<owner-id>` in the same transaction. Normal postings lock existing accounts only.

Deposit and spend requests accept `{"amount":100,"description":"Token purchase","external_source":"stripe","external_id":"invoice_123","metadata":{}}`; `external_source` and `external_id` are both optional but must occur together. Deposit uses `source:<external_source>` or `source:other` when absent. Both routes return `201` with `{"transaction_id":123,"owner_id":1,"available_balance":100}`. A duplicate pair always returns `409 duplicate_transaction`; it never returns a prior result.

Adjustment requests accept `{"description":"Manual correction","external_source":"admin","external_id":"adj_1","metadata":{},"postings":[{"account_code":"wallet:1","account_name":"Customer 1 Wallet","side":"debit","amount":10,"metadata":{}},{"account_code":"source:stripe","account_name":"Stripe Source","side":"credit","amount":10,"metadata":{}}]}` and return the same transaction result. Every referenced system account must already be registered. Balance returns `{"owner_id":1,"available_balance":100}`. Transaction reads return headers with entries ordered by entry ID. List requests accept `limit` (default 50, max 100) and an opaque base64 cursor encoding `(created_at,id)`; responses are `{"items":[...],"next_cursor":"..."}`. Invalid/malformed cursors return `400 validation_error`.

The server must refuse startup if `AURELIA_LEDGER_API_TOKEN` is empty. `/healthz` is intentionally public. `/v1/*` compares bearer tokens using `crypto/subtle.ConstantTimeCompare`; token rotation requires process restart in this initial release.

`POST /v1/owners/{ownerID}/reconcile` recomputes the wallet account from entries, writes that result to both `ledger_accounts.current_balance` and `ledger_owners.cached_balance` in one transaction, logs the old/new values, and returns `{"owner_id":1,"previous_available_balance":90,"available_balance":100,"repaired":true}`. It creates no journal entry because it repairs projections only.

`external_source` plus `external_id` is an optional idempotency key. For example, `stripe` + `invoice_123` identifies one payment notification. Both must be supplied together; a repeated pair must return `duplicate_transaction` and must not change balances.

### Tests and completion criteria

- Domain tests: posting delta/sign behavior and error classification.
- Integration tests: owner creation, every endpoint, auth failures, idempotent deposit retry, overspend rollback, balanced adjustment, metadata, transaction-history order, and reconciliation repair.
- Concurrency tests: simultaneous owner/account create, five deposits, two overspends against one wallet, and opposite-order multi-account adjustments.
- Document every endpoint with runnable `curl` examples. Phase completion requires `go test ./...`, `go test -race ./...`, and `go vet ./...` to pass.

## Phase 3 — Reservations, external work, and observability

**Objective:** add reserve/capture/release without allowing any reservation to settle more than its original amount.

### Data and workflow implementation

- Add `migrations/000003_reservation_settlement.sql` with one projection per reserve transaction: original amount, captured total, released total, status, and timestamps. Enforce non-negative totals and total settlement not greater than original amount.
- Extend `internal/ledger/service.go` with `Reserve`, `Capture`, `Release`, `GetReservation`, and `SpendWithOperation`.
- Reserve posts wallet credit plus reserved-wallet debit. Capture posts reserved-wallet credit plus `sink:consumed` debit. Release posts reserved-wallet credit plus available-wallet debit.
- Lock the reservation projection and reserved account in the same transaction before capture/release. Reject any request for which cumulative captured plus released would exceed the original reserve.
- `SpendWithOperation` must reserve, run caller work outside the database transaction, capture on success, and release on work failure. If release also fails, return both errors with `errors.Join` and log the recovery failure.

`SpendWithOperation` is not an HTTP route. It is an internal service method accepting a Go `ExternalWork` interface (`Execute(context.Context) error`) and is intended for future in-process provider adapters. The HTTP API exposes explicit reserve/capture/release only.

### HTTP and observability implementation

```text
POST /v1/owners/{ownerID}/reservations
POST /v1/reservations/{reservationID}/capture
POST /v1/reservations/{reservationID}/release
GET  /v1/reservations/{reservationID}
```

- Add `internal/observability` with injected `slog.Logger` and metrics interface. Log request ID, owner ID, transaction ID, reservation ID, operation, duration, and outcome; never log credentials or arbitrary raw metadata.
- Add optional `internal/outbox`: the transaction writes an event in the same commit; a worker leases, publishes, retries, and marks delivery. It starts only when `OUTBOX_ENABLED=true`.

The reservation projection has `reservation_transaction_id BIGINT PRIMARY KEY REFERENCES ledger_transactions(id) ON DELETE RESTRICT`, `owner_id`, `original_amount`, `captured_amount DEFAULT 0`, `released_amount DEFAULT 0`, and `status` (`open` or `settled`). Its status is `settled` only when `captured_amount + released_amount = original_amount`; otherwise it is `open`. Capture/release return the updated projection. A request amount omitted from the API means the complete remaining amount.

The optional outbox table has `id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY`, `event_type`, `transaction_id`, JSON `payload`, `attempts DEFAULT 0`, `available_at`, `lease_until`, `delivered_at`, and `last_error`. The only event types are `ledger.transaction.created` and `ledger.reservation.settled`. A worker leases up to 100 due undelivered events for 30 seconds, increments attempts, retries with capped exponential backoff, and marks delivery only after `Publisher.Publish` returns nil. There is no dead-letter table in this release; events remain queryable after 10 failed attempts and emit an error log.

### Tests and completion criteria

- Test full/partial capture/release, repeated settlement, capture after release, concurrent settlement, no overspend while funds are reserved, callback success/failure, and release failure after callback failure.
- Test outbox rollback, redelivery, and lease recovery when the outbox is enabled.
- Protect `/metrics` with the same bearer token in the first local deployment.

## Phase 4 — Operations, import, and release readiness

**Objective:** make the application deployable, inspectable, and safe to adopt.

### Components

- Add `cmd/reconcile` for read-only reports: unbalanced transactions, account/owner cached-balance drift, invalid reservation settlements, missing parent links, and duplicate idempotency keys.
- Add `--repair-owner-balance` only for mutable cached projections. It must print its planned change, require confirmation, and never edit journal headers/entries.
- Add `cmd/import` to load accounts, owners, transactions, entries, metadata, parent links, and idempotency keys from a documented PostgreSQL data file. Validate batches before commit and preserve supplied IDs where non-conflicting.
- Add Dockerfile, Compose configuration, `.env.example`, CI for formatting/test/race/vet, OpenAPI specification, deployment guide, backup/restore runbook, and load-test scripts.

### Acceptance and rollout

- Import into a disposable database, run reconciliation, verify zero unexplained drift, and execute API fixtures against imported data.
- During a controlled validation period, keep one writer authoritative and use this application for independent reads/reconciliation only. Do not permit two writers for the same accounts.
- Release only after database integration, concurrency, import, reconciliation, Docker startup, OpenAPI, and rollback checks pass.

`cmd/reconcile` defaults to read-only JSON output: `{"checked_transactions":0,"unbalanced_transactions":[],"account_drift":[],"owner_drift":[],"reservation_errors":[],"duplicate_idempotency_keys":[]}` and exits non-zero if any error list is non-empty. `--repair-owner-balance --owner-id <id> --confirm` is the only mutating form; without `--confirm` it prints the proposed values and exits without writes.

`cmd/import` accepts one JSON document with top-level arrays `owners`, `accounts`, `transactions`, and `entries`. It requires `--input <file>` and supports `--dry-run` and `--preserve-ids`. It validates referential links, entry positivity, per-transaction balance, metadata objects, and idempotency uniqueness before opening one write transaction. Any error rolls back the whole import. `--preserve-ids` rejects, rather than remaps, a colliding ID.

## Non-negotiable implementation rules

- Use `context.Context` for HTTP and database work and parameterized SQL exclusively.
- Handle PostgreSQL unique, deadlock, and serialization errors explicitly; retry only bounded safe transactions.
- Lock accounts in ascending code order; never use floats or mutate journal headers/entries.
- Do not trust caller-supplied owner IDs without bearer authentication and do not log credentials.
