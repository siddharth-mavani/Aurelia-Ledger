# TokenLedger

An HTTP application backed by PostgreSQL for recording double-entry token-ledger
transactions. It currently supports owner wallets, deposits, spends,
adjustments, reconciliation, and reservation/capture/release workflows.

## Quick start

1. Start a disposable PostgreSQL database and set `DATABASE_URL`, for example:

   ```bash
   docker run --rm --name token-ledger-postgres -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=tokenledger -p 5432:5432 postgres:16
   export DATABASE_URL='postgres://postgres:postgres@localhost:5432/tokenledger?sslmode=disable'
   ```

2. Apply schema migrations. This is required before starting the server and is safe to rerun:

   ```bash
   go run ./cmd/migrate up
   ```

3. Run tests:

   ```bash
   go test ./...
   go test -race ./...
   TOKEN_LEDGER_TEST_DATABASE_URL="$DATABASE_URL" go test ./tests/integration/...
   ```

4. Run the application and verify its health endpoint:

   ```bash
   export TOKENLEDGER_API_TOKEN='replace-with-a-local-secret'
   DATABASE_URL="$DATABASE_URL" go run ./cmd/server
   curl http://localhost:8080/healthz
   # {"status":"ok"}
   ```

Inspect the journal with `psql "$DATABASE_URL" -c '\d ledger_transactions'`.

## Domain contract

Amounts are positive integer token base units. Every transaction has balanced
debits and credits; an account balance is `sum(debits) - sum(credits)`. Journal
transactions and entries are append-only. Account cached balances are mutable
projection rows, updated by the posting operations introduced next.

## Phase 2 API

All `/v1/*` requests require `Authorization: Bearer $TOKENLEDGER_API_TOKEN`.

```bash
API=http://localhost:8080
AUTH="Authorization: Bearer $TOKENLEDGER_API_TOKEN"

curl -X POST "$API/v1/owners" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"type":"customer","external_ref":"alice@example.com","display_name":"Alice","metadata":{}}'

# Register a source before using it in a deposit.
curl -X POST "$API/v1/register-sources" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"stripe","display_name":"Stripe","metadata":{}}'

curl -X POST "$API/v1/owners/1/deposits" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"amount":100,"description":"Token purchase","external_source":"stripe","external_id":"invoice_123","metadata":{}}'

curl -X POST "$API/v1/owners/1/spends" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"amount":25,"description":"Token use","external_source":"app","external_id":"use_123","metadata":{}}'

curl -X POST "$API/v1/owners/1/adjustments" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"description":"Manual correction","metadata":{},"postings":[{"account_code":"wallet:1","account_name":"Owner 1 Wallet","side":"debit","amount":10,"metadata":{}},{"account_code":"source:stripe","account_name":"Stripe","side":"credit","amount":10,"metadata":{}}]}'

curl -H "$AUTH" "$API/v1/owners/1/balance"
curl -H "$AUTH" "$API/v1/owners/1/transactions?limit=50"
curl -H "$AUTH" "$API/v1/transactions/1"
curl -X POST -H "$AUTH" "$API/v1/owners/1/reconcile"
```

Wallet account codes are `wallet:<owner-id>`. System counterpart accounts are
`source:<name>` and `sink:<name>`; they are not owned by a ledger owner.

## Phase 3 reservations API

Reservations prevent funds from being spent twice while an operation is in
progress. Each owner has an available wallet and a reserved wallet:

```text
wallet:<owner-id>             available tokens
wallet:<owner-id>:reserved    held, not-yet-consumed tokens
```

The lifecycle is:

```text
reserve -> capture consumed tokens, or release unused tokens
```

For example, reserving 80 from an available balance of 100 leaves 20
available and 80 reserved. Capturing 30 and releasing with no amount consumes
30 and returns the remaining 50 to the available wallet.

```bash
# Hold 80 tokens for an in-progress provider operation.
curl -X POST "$API/v1/owners/1/reservations" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"amount":80,"description":"Provider hold","metadata":{}}'

# Example response includes reservation_transaction_id, such as 123.

# Capture requires an explicit positive amount.
curl -X POST "$API/v1/reservations/123/capture" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"amount":30,"description":"Provider completed work","metadata":{}}'

# An omitted release amount means release the complete remaining reservation.
curl -X POST "$API/v1/reservations/123/release" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{}'

curl -H "$AUTH" "$API/v1/reservations/123"
```

The server records a reservation projection with original, captured, and
released totals. PostgreSQL and the service enforce:

```text
captured_amount + released_amount <= original_amount
```

Capture and release lock the same reservation row before settlement, so two
concurrent requests cannot settle more than the original reservation. After
the totals reach the original amount, the reservation is `settled` and any
further capture or release is rejected.

All reservation routes use the same bearer token as the rest of `/v1/*`.
This is currently a local operator token: it authenticates the caller but does
not yet restrict that caller to a particular owner ID. Do not expose it as a
multi-user authorization scheme without adding owner/role authorization.

`SpendWithOperation` is intentionally not an HTTP route. It is an internal Go
service helper for a future in-process provider adapter: it commits a reserve,
runs external work outside the SQL transaction, captures on success, and
releases on failure. A local reservation does not mean an external provider or
bank accepted the operation.

The optional transactional outbox and metrics endpoint are not implemented in
this phase. See [learnings.md](learnings.md) for a detailed walkthrough of the
accounting movements, locking, test coverage, and real-world payment-flow
context.
