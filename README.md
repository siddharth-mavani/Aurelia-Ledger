# TokenLedger

An HTTP application backed by PostgreSQL for recording double-entry token-ledger
transactions. This project is in active development; the first release establishes
the durable journal, domain contract, and application health endpoint.

## Quick start

1. Start a disposable PostgreSQL database and set `DATABASE_URL`, for example:

   ```bash
   docker run --rm --name token-ledger-postgres -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=tokenledger -p 5432:5432 postgres:16
   export DATABASE_URL='postgres://postgres:postgres@localhost:5432/tokenledger?sslmode=disable'
   ```

2. Apply the schema in one transaction:

   ```bash
   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -1 -f migrations/000001_create_ledger.sql
   ```

3. Run tests:

   ```bash
   go test ./...
   go test -race ./...
   TOKEN_LEDGER_TEST_DATABASE_URL="$DATABASE_URL" go test ./tests/integration/...
   ```

4. Run the application and verify its health endpoint:

   ```bash
   DATABASE_URL="$DATABASE_URL" go run ./cmd/server
   curl http://localhost:8080/healthz
   # {"status":"ok"}
   ```

Inspect the journal with `psql "$DATABASE_URL" -c '\\d ledger_transactions'`.

## Domain contract

Amounts are positive integer token base units. Every transaction has balanced
debits and credits; an account balance is `sum(debits) - sum(credits)`. Journal
transactions and entries are append-only. Account cached balances are mutable
projection rows, updated by the posting operations introduced next.
