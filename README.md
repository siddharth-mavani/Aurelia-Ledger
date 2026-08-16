# Aurelia Ledger — A PostgreSQL-Backed Double-Entry Token Ledger

Aurelia Ledger is a locally runnable Go HTTP service for tracking token balances
without treating a balance as the source of truth. Every meaningful change is
recorded as an immutable, balanced journal transaction in PostgreSQL. Fast
wallet balances and reservation state are maintained as rebuildable
projections of that journal.

It is a learning-oriented project, but its core rules are deliberately close
to the rules a real token, credit, or payment ledger needs: positive integer
amounts, double-entry balancing, idempotency, row locking, and an audit trail
that cannot be rewritten.

## What You Can Do

- Create customer, team, or project owners; each receives a wallet account.
- Register token sources, then deposit tokens into an owner's wallet.
- Spend available tokens without allowing an overdraft.
- Make an explicit, balanced adjustment for a correction.
- Hold tokens in a reservation, then capture consumed tokens or release the
  unused amount.
- Retrieve a wallet balance, journal transaction, reservation, or paginated
  owner history.
- Reconcile a wallet's cached balance from the immutable journal.

## Explore

- [Run it locally](#run-it-locally)
- [See the system in one picture](#the-system-in-one-picture)
- [Follow the money](#follow-the-money)
- [Understand reservations](#reservations-hold-then-settle)
- [Learn the safety rules](#data-integrity-and-concurrency)
- [Use the HTTP API](#http-api)
- [Navigate the source](#source-map)

## Run It Locally

### Requirements

- Go 1.26 or newer (see [go.mod](go.mod) for the exact module settings)
- PostgreSQL 16+; Docker is the quickest option
- `curl` for the examples

### 1. Start PostgreSQL

This starts a disposable database on port `5432`:

```bash
docker run --rm --name aurelia-ledger-postgres \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=aurelia_ledger \
  -p 5432:5432 \
  postgres:16
```

In a second terminal, set the application configuration:

```bash
export DATABASE_URL='postgres://postgres:postgres@localhost:5432/aurelia_ledger?sslmode=disable'
export AURELIA_LEDGER_API_TOKEN='local-development-secret'
export API='http://localhost:8080'
export AUTH="Authorization: Bearer $AURELIA_LEDGER_API_TOKEN"
```

`DATABASE_URL` is required by both commands. `AURELIA_LEDGER_API_TOKEN` is
required by the server and protects every `/v1/*` route. `LISTEN_ADDR` is
optional and defaults to `:8080`.

### 2. Create the schema

```bash
go run ./cmd/migrate up
```

The migration runner keeps a `schema_migrations` record and uses a PostgreSQL
advisory lock, so rerunning the command applies only migrations that have not
already been recorded. The server intentionally does **not** migrate the
database for you.

### 3. Start the service

```bash
go run ./cmd/server
```

Check that the process can reach PostgreSQL:

```bash
curl "$API/healthz"
# {"status":"ok"}
```

`/healthz` is public. It verifies database reachability; it does not prove
that migrations have been applied or that a posting can succeed.

### 4. Run the tests

```bash
go test ./...
go test -race ./...
go vet ./...

# This integration suite creates and drops tables: use a disposable database.
AURELIA_LEDGER_TEST_DATABASE_URL="$DATABASE_URL" go test ./tests/integration/...
```

If your environment cannot write Go's default build cache, use:

```bash
GOCACHE=/private/tmp/aurelia_ledger_gocache go test ./...
```

## The System in One Picture

```mermaid
flowchart LR
    Client[Client or local operator] -->|Bearer-authenticated request| API[Go HTTP API]
    API --> Service[Ledger service]
    Service -->|one SQL transaction| Store[(PostgreSQL)]

    subgraph LedgerDB[PostgreSQL ledger]
        Owners[Owners\nand wallet accounts]
        Journal[Immutable transaction\nheaders and entries]
        Projections[Mutable balance\nand reservation projections]
    end

    Service --> Owners
    Service --> Journal
    Service --> Projections
    Journal -.->|rebuild / reconcile| Projections
```

The journal is the durable explanation of *why* a balance changed. Projection
rows exist to make common reads and reservation settlement efficient, but they
are not a replacement for journal history.

Every owner has an available wallet:

```text
owner 42  ──>  wallet:42
```

Reservation operations also use a second, owner-specific account:

```text
owner 42  ──>  wallet:42             available tokens
              wallet:42:reserved    held tokens
```

System counterpart accounts use `source:<name>` and `sink:<name>` codes. They
are ledger accounts, but do not belong to an owner. The built-in accounts are
`source:other`, `sink:spend`, and `sink:consumed`; other deposit sources must
be registered first.

## Follow the Money

### The accounting model

Amounts are positive integer token base units. A transaction consists of two
or more entries, and its debit total must equal its credit total:

```text
account balance = sum(debit entries) - sum(credit entries)
```

For example, a deposit of 100 tokens from Stripe records two entries:

```mermaid
flowchart LR
    Stripe[source:stripe] -->|credit 100| Txn[Deposit journal transaction]
    Txn -->|debit 100| Wallet[wallet:1]
    Wallet --> Balance[Available balance: +100]
```

The arrow describes the value movement for this example; the journal stores
the precise debit and credit entries. A direct spend reverses the wallet side:

```mermaid
flowchart LR
    Wallet[wallet:1] -->|credit 25| Txn[Spend journal transaction]
    Txn -->|debit 25| Sink[sink:spend]
    Wallet --> Balance[Available balance: -25]
```

### A complete first interaction

Create an owner, register a source, deposit tokens, and read the resulting
available balance:

```bash
curl -X POST "$API/v1/owners" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"type":"customer","external_ref":"alice@example.com","display_name":"Alice","metadata":{}}'

curl -X POST "$API/v1/register-sources" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"stripe","display_name":"Stripe","metadata":{}}'

curl -X POST "$API/v1/owners/1/deposits" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"amount":100,"description":"Token purchase","external_source":"stripe","external_id":"invoice_123","metadata":{}}'

curl -H "$AUTH" "$API/v1/owners/1/balance"
# {"owner_id":1,"available_balance":100}
```

`external_source` and `external_id` form one idempotency key: supply both or
neither. When they are supplied, the same pair cannot create another journal
transaction. Use a unique pair for every distinct externally initiated event.

### A standard posting workflow

```mermaid
sequenceDiagram
    participant C as Client
    participant A as HTTP API
    participant S as Ledger service
    participant P as PostgreSQL

    C->>A: POST deposit, spend, or adjustment
    A->>A: Validate bearer token and JSON body
    A->>S: Build command
    S->>P: Begin transaction and lock owner/accounts
    S->>S: Validate postings and available funds
    S->>P: Insert immutable transaction and entries
    S->>P: Update cached balances
    P-->>S: Commit or roll back everything
    S-->>A: transaction ID and available balance
    A-->>C: 201 Created
```

An adjustment is intentionally explicit: provide the balanced posting lines
yourself. It is useful for corrections because the project never edits or
deletes the original journal rows.

## Reservations: Hold, Then Settle

A reservation moves money out of the available wallet before external work
begins. It prevents the same tokens from being spent twice while that work is
in progress.

```mermaid
stateDiagram-v2
    [*] --> Open: reserve
    Open --> Open: capture part\nor release part
    Open --> Settled: captured + released = original
    Settled --> [*]
```

With 100 available tokens, reserving 80 leaves 20 available and 80 held:

```mermaid
flowchart LR
    AvailableBefore[wallet:1\n100 available] -->|reserve 80| AvailableAfter[wallet:1\n20 available]
    AvailableBefore -->|reserve 80| Reserved[wallet:1:reserved\n80 held]
    Reserved -->|capture 30| Consumed[sink:consumed\n30 consumed]
    Reserved -->|release 50| AvailableFinal[wallet:1\n70 available]
```

The corresponding API calls are:

```bash
# Hold 80 tokens. A reservation record is returned.
curl -X POST "$API/v1/owners/1/reservations" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"amount":80,"description":"Provider hold","external_source":"provider","external_id":"hold_123","metadata":{}}'

# Replace 123 with reservation_transaction_id from the response.
curl -X POST "$API/v1/reservations/123/capture" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"amount":30,"description":"Provider completed work","external_source":"provider","external_id":"capture_123","metadata":{}}'

# No amount releases all of the reservation that remains.
curl -X POST "$API/v1/reservations/123/release" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{}'

curl -H "$AUTH" "$API/v1/reservations/123"
```

For each reservation, the service and database require:

```text
captured_amount + released_amount <= original_amount
```

Capture and release lock the same reservation row. Therefore concurrent
settlement requests see a consistent remaining amount and cannot settle more
than was originally reserved. Once the combined total reaches the original
amount, the reservation becomes `settled` and further settlement is rejected.

`SpendWithOperation` is also available as an internal Go service helper. It
reserves first, runs a supplied external callback outside the SQL transaction,
then captures after success or releases after failure. A successful local
reservation never proves that an external provider accepted the operation.

## Data Integrity and Concurrency

| Rule | How it is protected |
| --- | --- |
| Every posting balances | Go validates entries before writing; a deferred PostgreSQL constraint trigger verifies it again at commit. |
| Journal history is append-only | PostgreSQL triggers reject updates and deletes of transaction headers and entries. |
| Amounts are positive | Domain validation and the `ledger_entries.amount > 0` check. |
| A wallet cannot overspend | The service locks relevant accounts, calculates signed deltas, and rejects a negative available or reserved balance where required. |
| One external event is posted once | A paired `external_source`/`external_id` key plus a partial unique index. |
| A wallet belongs to its owner | Canonical `wallet:<owner-id>` codes, service checks, and owner foreign keys. |
| Reservation settlement stays within the hold | Row locking plus database checks on original, captured, and released amounts. |
| Cached balances can be repaired | Reconciliation recomputes the available wallet from journal entries in one transaction. |

The service locks existing account rows in sorted account-code order. That
consistent order avoids the usual circular wait when two requests need the
same pair of accounts, although a request may still wait for another request
to finish.

## HTTP API

All `/v1/*` endpoints require:

```http
Authorization: Bearer <AURELIA_LEDGER_API_TOKEN>
```

The current bearer token authenticates a local operator. It does **not** yet
authorize that operator for a specific owner, so do not expose it as a
multi-user authorization system.

| Method | Route | Purpose |
| --- | --- | --- |
| `GET` | `/healthz` | Public PostgreSQL connectivity check. |
| `POST` | `/v1/owners` | Create an owner and its available wallet. |
| `POST` | `/v1/register-sources` | Create a reusable `source:<name>` account. |
| `POST` | `/v1/owners/{id}/deposits` | Credit tokens to an owner's available wallet. |
| `POST` | `/v1/owners/{id}/spends` | Spend tokens from the available wallet. |
| `POST` | `/v1/owners/{id}/adjustments` | Post caller-supplied balanced correction entries. |
| `GET` | `/v1/owners/{id}/balance` | Read the cached available balance. |
| `GET` | `/v1/owners/{id}/transactions?limit=50&cursor=...` | Read owner history; `limit` is 1–100 and the response includes `next_cursor`. |
| `GET` | `/v1/transactions/{id}` | Read one journal transaction and its entries. |
| `POST` | `/v1/owners/{id}/reconcile` | Rebuild the owner's available balance projection from the journal. |
| `POST` | `/v1/owners/{id}/reservations` | Move available tokens into a reservation. |
| `GET` | `/v1/reservations/{id}` | Read one reservation projection. |
| `POST` | `/v1/reservations/{id}/capture` | Move held tokens to `sink:consumed`. |
| `POST` | `/v1/reservations/{id}/release` | Return held tokens to the available wallet. |

Requests must be JSON objects and unknown JSON fields are rejected. API errors
use this shape:

```json
{
  "error": {
    "code": "validation_error",
    "message": "..."
  }
}
```

Common outcomes are `400` for invalid input, `401` for missing or incorrect
authentication, `404` for a missing owner/account/transaction/reservation,
and `409` for duplicate events, insufficient funds, or an over-settlement.

## Inspect the Database

The three central journal tables are:

```text
ledger_accounts      account identity and current balance projection
ledger_transactions  immutable event header: type, owner, idempotency key
ledger_entries       immutable debit/credit lines for each transaction
```

Owners live in `ledger_owners`. Reservation progress lives in
`ledger_reservations`; it is a mutable settlement projection linked to its
original reserve transaction.

Useful local queries:

```bash
psql "$DATABASE_URL" -c '\d ledger_transactions'
psql "$DATABASE_URL" -c 'SELECT id, code, current_balance FROM ledger_accounts ORDER BY id'
psql "$DATABASE_URL" -c 'SELECT id, transaction_type, owner_id, description, created_at FROM ledger_transactions ORDER BY id'
```

For a concise table-by-table list of schema protections, see the
[schema integrity reference](docs/phase-1-parity.md). For a beginner-friendly
walkthrough of Go, SQL, transactions, and the code paths, see
[learnings.md](learnings.md).

## Source Map

```mermaid
flowchart TD
    Server[cmd/server] --> Config[internal/config]
    Server --> API[internal/httpapi]
    API --> Service[internal/ledger/service.go]
    Service --> Repos[internal/database/postgres]
    Repos --> PostgreSQL[(PostgreSQL)]
    Domain[internal/domain] --> API
    Domain --> Service
    Migrate[cmd/migrate] --> Migrator[internal/database/postgres/migrator.go]
    Migrator --> PostgreSQL
```

| Location | Responsibility |
| --- | --- |
| [cmd/server](cmd/server/main.go) | Loads configuration, opens PostgreSQL, starts and gracefully shuts down HTTP. |
| [cmd/migrate](cmd/migrate/main.go) | Applies recorded SQL migrations. |
| [internal/httpapi](internal/httpapi/api.go) | Routes requests, authenticates the bearer token, parses JSON, and maps errors to HTTP responses. |
| [internal/ledger](internal/ledger/service.go) | Coordinates owner, posting, balance, reservation, and reconciliation workflows. |
| [internal/domain](internal/domain) | Defines amounts, posting validation, transaction types, metadata, and domain errors. |
| [internal/database/postgres](internal/database/postgres) | Encapsulates the PostgreSQL store, migration runner, repositories, locking, and persistence queries. |
| [migrations](migrations) | Defines tables, indexes, constraints, triggers, built-in accounts, and reservation checks. |
| [tests](tests) and package tests | Covers domain behavior, HTTP handling, repositories, locking helpers, and opt-in PostgreSQL integration behavior. |

## Current Scope

This repository is designed for local development and learning. It includes
durable journal posting, idempotency, owner wallets, reservation settlement,
and local-token authentication. It does not provide a production identity
system, an HTTP metrics endpoint, a transactional outbox, or a guarantee that
an external provider action and local settlement are one atomic event.
