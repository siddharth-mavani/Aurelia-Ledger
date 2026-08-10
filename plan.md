# TokenLedger application implementation plan

## Architecture and conventions

TokenLedger is a locally runnable HTTP application backed by PostgreSQL. The server starts in `cmd/server`; domain rules live in `internal/domain`; database access and SQL transaction boundaries live in `internal/database/postgres`; HTTP code belongs in `internal/httpapi`; application workflows belong in `internal/ledger`.

All tests live below `tests/`: deterministic unit tests in `tests/domain` and disposable-PostgreSQL API/database tests in `tests/integration`. PostgreSQL is the only runtime database. Amounts are positive `int64` token base units. Account balance is `sum(debits) - sum(credits)`. Journal headers and entries are append-only; account/owner balances are mutable projections updated in the same transaction as their postings.

## Phase 1 — Application and durable journal foundation

**Status:** completed.

### Implemented components

- `cmd/server/main.go`: process entrypoint, structured logging, PostgreSQL startup check, graceful SIGINT/SIGTERM shutdown, and `GET /healthz`.
- `internal/config`: reads mandatory `DATABASE_URL` and optional `LISTEN_ADDR` (default `:8080`), without logging credentials.
- `internal/domain`: `Amount`, JSON-object `Metadata`, debit/credit `EntrySide`, `TransactionType`, `OwnerRef`, `IdempotencyKey`, persisted records, posting validation, and typed errors.
- `internal/database/postgres`: `pgx` database connection and `database/sql` transaction helper.
- `migrations/000001_create_ledger.sql`: accounts, transaction headers, entries, indexes, JSON/type/amount constraints, immutable journal triggers, and deferred commit-time balance checks.

### Validation and local execution

```bash
cd /Users/smavani/dev/token-ledger
GOCACHE=/private/tmp/tokenledger_gocache go test ./...
GOCACHE=/private/tmp/tokenledger_gocache go test -race ./...
GOCACHE=/private/tmp/tokenledger_gocache go vet ./...

psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -1 -f migrations/000001_create_ledger.sql
DATABASE_URL="$DATABASE_URL" go run ./cmd/server
curl http://localhost:8080/healthz
```

Set `TOKEN_LEDGER_TEST_DATABASE_URL` to a disposable PostgreSQL database to execute the opt-in schema integration tests.

## Phase 2 — Owner model, posting service, and HTTP API

**Objective:** make the application create owners and execute deposit, spend, adjustment, history, balance, and reconciliation workflows.

### Database work

- Add `migrations/000002_create_owners.sql` with `ledger_owners`: identity ID, owner type, unique external reference within type, display name, cached available balance, JSON metadata, and timestamps.
- Add `ledger_accounts.owner_id` and owner-account indexes. Wallet accounts use one owner; source and sink accounts remain system-owned. Retain `owner_type` on transaction headers so the model can support customer, team, and project ownership.
- Add foreign-key/index migrations only in forward-compatible steps; do not modify the Phase-1 migration after it has been applied anywhere.

### Service and database implementation

- Add `internal/database/postgres/owner_repository.go` and `ledger_repository.go`. All queries use parameters, `context.Context`, and a caller-provided `*sql.Tx` for writes.
- Add `internal/ledger/service.go` with typed commands/results: `CreateOwner`, `Deposit`, `Spend`, `Adjust`, `GetBalance`, `ListTransactions`, `GetTransaction`, `CalculateBalance`, and `ReconcileOwner`.
- Add `internal/ledger/posting.go` as the sole journal-write path. It validates a command and `ValidatePostings`, starts one transaction, creates/fetches accounts, locks rows by ascending account code using `SELECT ... FOR UPDATE`, applies cache deltas, inserts header/entries, updates the owner available balance, and commits.
- `OwnerRef` on every owner-scoped transaction links the header to its owner for history queries, wallet lookup, authorization, and cached balance updates.
- Use `EnforcePositive=true` for wallet credits in spend operations. Keep it false in adjustments so explicit corrective/reversal postings can create a negative account balance when necessary.

### HTTP API

Add `internal/httpapi` for routing, JSON decoding, request IDs, validation, and stable JSON errors. Require `Authorization: Bearer <token>` for `/v1/*`; load the local operator token only from `TOKENLEDGER_API_TOKEN` and never log it.

```text
POST /v1/owners
POST /v1/owners/{ownerID}/deposits
POST /v1/owners/{ownerID}/spends
POST /v1/owners/{ownerID}/adjustments
GET  /v1/owners/{ownerID}/balance
GET  /v1/owners/{ownerID}/transactions
GET  /v1/transactions/{transactionID}
POST /v1/owners/{ownerID}/reconcile
```

Return `validation_error` (400), `unauthorized` (401), `not_found` (404), `insufficient_funds` (409), `duplicate_transaction` (409), or `internal_error` (500).

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

### HTTP and observability implementation

```text
POST /v1/owners/{ownerID}/reservations
POST /v1/reservations/{reservationID}/capture
POST /v1/reservations/{reservationID}/release
GET  /v1/reservations/{reservationID}
```

- Add `internal/observability` with injected `slog.Logger` and metrics interface. Log request ID, owner ID, transaction ID, reservation ID, operation, duration, and outcome; never log credentials or arbitrary raw metadata.
- Add optional `internal/outbox`: the transaction writes an event in the same commit; a worker leases, publishes, retries, and marks delivery. It starts only when `OUTBOX_ENABLED=true`.

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

## Non-negotiable implementation rules

- Use `context.Context` for HTTP and database work and parameterized SQL exclusively.
- Handle PostgreSQL unique, deadlock, and serialization errors explicitly; retry only bounded safe transactions.
- Lock accounts in ascending code order; never use floats or mutate journal headers/entries.
- Do not trust caller-supplied owner IDs without bearer authentication and do not log credentials.
