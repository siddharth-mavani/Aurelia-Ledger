# TokenLedger learning guide

This guide explains the current TokenLedger implementation for somebody new to
Go, HTTP applications, and PostgreSQL. It covers the code that exists today,
not only the features planned for later phases.

## What the application does now

TokenLedger is an HTTP application with a PostgreSQL-backed ledger foundation.
Today it can read configuration, connect to PostgreSQL, serve GET /healthz, and
define/test database rules for future ledger writes.

It does **not** yet expose an HTTP API to create accounts, deposits, spends, or
history. That work is planned in Phase 2 in plan.md.

~~~text
GET /healthz returning 200 {"status":"ok"}
means PostgreSQL is reachable.

It does not prove the database migration was applied.
It does not write a ledger transaction.
~~~

## How files connect

~~~text
cmd/server/main.go
  -> internal/config/config.go
       reads DATABASE_URL and LISTEN_ADDR
  -> internal/database/postgres/store.go
       creates a Go database pool and pings PostgreSQL
  -> starts the HTTP server
       -> GET /healthz
            -> PingContext on PostgreSQL
            -> 200 JSON response, or HTTP 503 when unavailable

Future write path:
HTTP request -> domain validation -> SQL transaction -> COMMIT
                                       -> PostgreSQL verifies balance
~~~

| Path | Purpose |
| --- | --- |
| go.mod | Module name and direct dependency on pgx. |
| go.sum | Download checksums maintained by Go. |
| cmd/server/main.go | Runnable server and health route. |
| internal/config/config.go | Reads configuration from environment variables. |
| internal/database/postgres/store.go | Database-pool and transaction helper. |
| internal/domain/types.go | Ledger types and early Go validation. |
| internal/domain/errors.go | Named errors used by validation/tests. |
| migrations/000001_create_ledger.sql | Tables, constraints, indexes, and triggers. |
| tests/domain/types_test.go | Unit tests with no database. |
| tests/integration/postgres_test.go | Optional real-PostgreSQL test. |
| README.md | Setup and run commands. |
| docs/phase-1-parity.md | Rule-to-enforcement reference. |
| plan.md | Current and future scope. |

## Go module, packages, and dependencies

go.mod contains:

~~~go
module tokenledger
~~~

That is the import prefix for this repository:

~~~go
import "tokenledger/internal/config"
~~~

A package is a group of Go files in one directory using the same package name.

~~~text
Directory                              Package
internal/config                        config
internal/database/postgres             postgres
internal/domain                        domain
cmd/server                             main
~~~

The internal directory has special Go protection: only code inside this module
can import it. The server directory uses package main and func main(), making
it a runnable program. The other packages are libraries.

pgx is the direct PostgreSQL library. Its own required libraries are normally
added automatically to go.mod as indirect requirements. Do not normally add
them manually; run go mod tidy to synchronize dependencies with imports.

## Configuration

The Config struct groups configuration values:

~~~go
type Config struct {
    DatabaseURL string
    ListenAddr  string
}
~~~

A struct is a value with named fields. Load reads:

~~~go
DatabaseURL: os.Getenv("DATABASE_URL"),
ListenAddr:  os.Getenv("LISTEN_ADDR"),
~~~

DATABASE_URL is required. LISTEN_ADDR is optional and defaults to :8080.

~~~text
postgres://postgres:postgres@localhost:5432/tokenledger?sslmode=disable
           username password host      port  database name
~~~

This URL is appropriate for a disposable local database only. Production
credentials should be protected and production connections should use TLS.

## main.go: Go syntax, server startup, and shutdown

~~~go
logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
~~~

The := operator means declare a new variable and assign a value.

Many Go functions return a value and an error:

~~~go
config, err := config.Load()
~~~

The convention is:

~~~text
value, error
success -> error is nil
failure -> error has an explanation
~~~

If configuration is invalid, the program logs the problem and exits with
status 1. A non-zero status tells a shell/deployment system that startup failed.

### Context

~~~go
ctx, stop := signal.NotifyContext(
    context.Background(), os.Interrupt, syscall.SIGTERM,
)
defer stop()
~~~

ctx is a variable name with type context.Context. A context carries
lifetime/cancellation information down through function calls.

If the user presses Ctrl+C or the OS sends SIGTERM, ctx is cancelled. Database
and HTTP work using that context can stop instead of continuing after shutdown
has begun. context.Background() is an empty root context with no deadline.

defer means run this cleanup call when the surrounding function returns.

~~~go
<-ctx.Done()
~~~

Done returns a channel that becomes ready when cancellation happens. The <- is
a receive/wait operation. Then the server gets up to ten seconds to finish
active HTTP requests with server.Shutdown.

### HTTP route

~~~go
mux := http.NewServeMux()
mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
    // handler logic
})
~~~

- ServeMux is an HTTP router.
- HandleFunc maps an HTTP method/path to a function.
- w is how a handler writes a response.
- r is the incoming request.
- func(...) {...} is an anonymous function, written directly where it is used.

The handler calls:

~~~go
store.DB().PingContext(r.Context())
~~~

r.Context is the context of that individual HTTP request. A disconnected client
can cancel the PostgreSQL ping. On success, the handler encodes a Go dictionary:

~~~go
_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
~~~

map[string]string means a map/dictionary with string keys and string values.
The _ means intentionally ignore Encode's returned error.

## PostgreSQL driver and blank import

store.go imports:

~~~go
import (
    "database/sql"
    _ "github.com/jackc/pgx/v5/stdlib"
)
~~~

database/sql is Go's generic SQL API. It does not itself know PostgreSQL's
network protocol. A database driver is the adapter that does:

~~~text
Your Go code -> database/sql -> pgx driver -> PostgreSQL server
~~~

The _ is a blank import. It means:

> Load this package and run its startup side effects, but do not use its
> package name directly in this source file.

The pgx stdlib adapter registers itself during Go program startup. A simplified
mental model is:

~~~go
func init() {
    sql.Register("pgx", aPostgresDriver)
}
~~~

Therefore:

~~~go
sql.Open("pgx", databaseURL)
~~~

means use the Go-side driver registered under the name pgx to open this URL.
It does not rename PostgreSQL, the database service, or the database itself.

## store.go explained

~~~go
type Store struct{ db *sql.DB }
~~~

Expanded form:

~~~go
type Store struct {
    db *sql.DB
}
~~~

Store is a struct with one private field. *sql.DB is a pointer to Go's
database pool. It is not a single connection; it is a concurrency-safe pool of
connections used by database operations.

~~~go
func Open(ctx context.Context, databaseURL string) (*Store, error)
~~~

- func defines a function.
- Open starts with a capital letter, so other packages can call it.
- ctx context.Context is a parameter named ctx.
- databaseURL string is a string parameter.
- (*Store, error) says the function returns two values.

~~~go
db, err := sql.Open("pgx", databaseURL)
~~~

This declares db and err. Open configures a database pool, but may not make a
network connection immediately.

~~~go
if err := db.PingContext(ctx); err != nil {
    db.Close()
    return nil, fmt.Errorf("ping postgres: %w", err)
}
~~~

This is an if initializer: err is created just for this if. PingContext checks
that PostgreSQL is reachable now. On failure, Close releases pool resources;
nil means no Store; and %w wraps the original error while retaining its cause.

~~~go
return &Store{db: db}, nil
~~~

Store{db: db} creates a struct. & takes its address, producing a *Store.
nil as the error means success.

~~~go
func (s *Store) DB() *sql.DB  { return s.db }
func (s *Store) Close() error { return s.db.Close() }
~~~

These are methods attached to Store. (s *Store) is the receiver: inside the
method, s is the Store in use. The pointer receiver avoids copying it.

### Database transactions

~~~go
func (s *Store) WithinTransaction(
    ctx context.Context,
    fn func(*sql.Tx) error,
) error
~~~

fn func(*sql.Tx) error means the caller supplies a function that receives a
transaction handle and returns an error.

~~~go
err := store.WithinTransaction(ctx, func(tx *sql.Tx) error {
    _, err := tx.ExecContext(ctx, "INSERT INTO ...")
    return err
})
~~~

WithinTransaction organizes this:

~~~text
BEGIN
  run caller SQL
  callback returns error -> ROLLBACK
  callback succeeds      -> COMMIT
~~~

The Go helper organizes the unit of work. PostgreSQL is the authority that
actually commits/rolls back atomically and enforces deferred constraints.

The line _ = tx.Rollback() intentionally ignores a possible rollback error,
because the callback's original SQL error is normally the useful one.

## Ledger database model

The migration creates three tables:

~~~text
ledger_accounts     Accounts where token balances are recorded
ledger_transactions One event, called the transaction header
ledger_entries      Debit/credit lines for that event
~~~

Example: customer 42 receives 100 tokens through Stripe.

~~~text
ledger_accounts
  id=1  code=wallet:42       name=User 42 Wallet
  id=2  code=source:stripe   name=Stripe Token Source

ledger_transactions
  id=101 type=deposit description=invoice
  external_source=stripe external_id=inv-42

ledger_entries
  account_id=1 transaction_id=101 entry_type=debit  amount=100
  account_id=2 transaction_id=101 entry_type=credit amount=100
~~~

The transaction header is the ledger_transactions row. It stores details for
the overall event: type, description, optional owner, optional parent,
external idempotency key, metadata, and timestamps. It does not store amounts,
because one event can have two or more entry lines.

The main accounting rule is:

~~~text
There must be at least one entry.
Total debit amount must equal total credit amount.
~~~

Amounts are positive integers; entry_type determines whether each amount is a
debit or credit.

## Table rules: primary keys, foreign keys, checks

~~~sql
id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY
~~~

- BIGINT is a 64-bit integer.
- IDENTITY means PostgreSQL creates the ID.
- PRIMARY KEY means unique stable identity.

~~~sql
code TEXT NOT NULL UNIQUE
~~~

- TEXT stores strings.
- NOT NULL requires a value.
- UNIQUE rejects duplicates.

current_balance is a mutable cached summary for fast reads. Entry rows remain
the durable accounting history.

Entries use foreign keys:

~~~sql
account_id BIGINT NOT NULL REFERENCES ledger_accounts(id) ON DELETE RESTRICT
transaction_id BIGINT NOT NULL REFERENCES ledger_transactions(id) ON DELETE RESTRICT
~~~

A foreign key links a child row to an existing row in another table.
PostgreSQL rejects entries whose account/transaction does not exist and blocks
deletion of rows still referenced by entries. ON DELETE RESTRICT protects the
audit trail.

CHECK constraints are rules evaluated by PostgreSQL before accepting a row:

~~~sql
CHECK (amount > 0)
CHECK (entry_type IN ('debit', 'credit'))
~~~

They protect the data even if a script bypasses the Go application.

## Indexes and idempotency

An index is like a book index: it helps PostgreSQL find relevant rows without
reading every row in a table.

~~~sql
CREATE INDEX ledger_entries_account_created_at_idx
ON ledger_entries(account_id, created_at DESC);
~~~

This helps queries asking for one account's entries from newest to oldest.
Indexes make reads faster, but take disk space and add a little write work.

Idempotency means retrying the same external event does not record it twice.
The key is the pair:

~~~text
(external_source, external_id)
~~~

The migration uses:

~~~sql
CREATE UNIQUE INDEX ledger_transactions_external_source_id_key
ON ledger_transactions(external_source, external_id)
WHERE external_source IS NOT NULL;
~~~

UNIQUE rejects duplicate supplied pairs. WHERE makes this a partial index:
internal transactions can have both fields absent.

An application-only SELECT then INSERT check is unsafe under concurrency: two
requests can both see no row and then both insert. A table UNIQUE constraint is
an alternate syntax, but PostgreSQL still creates a unique index behind the
scenes. A safe database-level uniqueness/idempotency guarantee is index-backed.

## Triggers and immutable journal history

A trigger is PostgreSQL code that runs automatically because a database event
occurred:

~~~sql
CREATE TRIGGER ledger_entries_immutable
BEFORE UPDATE OR DELETE ON ledger_entries
FOR EACH ROW EXECUTE FUNCTION ledger_reject_journal_mutation();
~~~

Read it as:

> Before each individual ledger entry row is updated or deleted, call
> ledger_reject_journal_mutation().

- BEFORE: run before the change.
- UPDATE OR DELETE: watched events.
- ON ledger_entries: watched table.
- FOR EACH ROW: run once per affected row.
- EXECUTE FUNCTION: call the named PostgreSQL function.

The alternate, FOR EACH STATEMENT, runs once for the entire SQL statement.
The immutable-journal function raises an exception. Headers and entries are
append-only: correct errors with a new compensating transaction, never by
rewriting historical rows.

Inside trigger functions, PostgreSQL provides special values:

~~~text
NEW             inserted/updated row
OLD             pre-update/deletion row
TG_TABLE_NAME   table that fired the trigger
~~~

## Deferred balance checks

The schema attaches ledger_assert_transaction_balanced to both headers and
entries:

~~~sql
AFTER INSERT ... DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION ledger_assert_transaction_balanced();
~~~

AFTER INSERT means the row is inserted first. DEFERRABLE INITIALLY DEFERRED
means PostgreSQL waits until COMMIT to execute the check. This permits a header
to be inserted before its entries refer to its ID.

~~~text
BEGIN
  insert transaction header, ID 101
  insert debit entry, amount 100
  insert credit entry, amount 100
COMMIT -> PostgreSQL accepts because 100 debit equals 100 credit
~~~

Both attachments are needed:

~~~text
Header trigger: rejects a new header that reaches COMMIT with zero entries.
Entry trigger: rejects a new entry added later to an existing balanced
               transaction if it would make totals unequal.
~~~

The intended Go path will write all rows in one transaction, but the database
must protect data from every future code path, maintenance script, or direct
SQL command.

## Go domain validation

internal/domain/types.go validates future write input before SQL is executed.

~~~go
type Amount int64
type EntrySide string
type TransactionType string
type Metadata json.RawMessage
~~~

Go does not have conventional enums. This project uses named string types plus
constants:

~~~go
const (
    Debit  EntrySide = "debit"
    Credit EntrySide = "credit"
)
~~~

Amount.Validate rejects zero/negative values. Metadata.Validate accepts empty
metadata or a JSON object such as {"plan":"pro"}, but rejects an array such as
[].

Posting is unpersisted input for a future write operation. []Posting is a
slice: a variable-length, ordered collection of Posting values.

~~~go
postings := []Posting{
    {AccountCode: "wallet:42", Side: Debit, Amount: 100},
    {AccountCode: "source:stripe", Side: Credit, Amount: 100},
}
~~~

It differs from [2]Posting, which is a fixed-size array with exactly two
elements. ValidatePostings requires a non-empty slice, individually valid
postings, and equal debit/credit totals.

## Tests and useful commands

Run ordinary tests:

~~~bash
go test ./...
go test -race ./...
~~~

If the default Go cache is unavailable in a restricted environment:

~~~bash
GOCACHE=/private/tmp/token-ledger_gocache go test ./...
~~~

The domain tests need no database. The integration test is opt-in:

~~~bash
TOKEN_LEDGER_TEST_DATABASE_URL="$DATABASE_URL" go test ./tests/integration/...
~~~

It drops/recreates tables and functions, so its URL must point to an empty,
disposable database. Never point it at important data.

Start local PostgreSQL:

~~~bash
docker run --rm --name token-ledger-postgres \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=tokenledger \
  -p 5432:5432 postgres:16
~~~

Then, in another terminal:

~~~bash
cd /Users/smavani/dev/token-ledger
export DATABASE_URL='postgres://postgres:postgres@localhost:5432/tokenledger?sslmode=disable'
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -1 -f migrations/000001_create_ledger.sql
go run ./cmd/server
curl http://localhost:8080/healthz
~~~

psql -1 applies the migration in one database transaction. If a statement
fails, PostgreSQL rolls back the complete migration.

## Core takeaways

~~~text
Go provides typed values, early validation, HTTP, and transaction orchestration.
PostgreSQL is the durable source of truth and enforces final data integrity.
Health checks prove connectivity, not ledger writes.
Headers describe events; entries record debit/credit lines.
Headers and entries are append-only.
PostgreSQL checks balance at COMMIT after it can see all entries.
~~~
