# Aurelia Ledger learning guide

This guide explains the current Aurelia Ledger implementation for somebody new to
Go, HTTP applications, and PostgreSQL. It covers the code that exists today,
not only the features planned for later phases.

## What the application does now

Aurelia Ledger is an HTTP application with a PostgreSQL-backed token ledger. It
can create owners, register trusted deposit sources, record deposits, spends,
and adjustments, read balances/history, and reconcile cached projections.

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

cmd/migrate/main.go
  -> internal/config/config.go
       reads DATABASE_URL
  -> internal/database/postgres/migrator.go
       reads ordered SQL files, locks PostgreSQL, and applies missing files

Write path:
HTTP request -> authentication -> domain/service validation -> SQL transaction
  -> lock owner and affected accounts -> write journal and projections -> COMMIT
                                                               -> PostgreSQL verifies balance
~~~

| Path | Purpose |
| --- | --- |
| go.mod | Module name and direct dependency on pgx. |
| go.sum | Download checksums maintained by Go. |
| cmd/server/main.go | Runnable server and health route. |
| cmd/migrate/main.go | Runnable schema-migration command; it accepts `up`. |
| internal/config/config.go | Reads configuration from environment variables. |
| internal/database/postgres/store.go | Database-pool and transaction helper. |
| internal/database/postgres/migrator.go | Finds, locks, applies, and records schema migrations. |
| internal/domain/types.go | Ledger types and early Go validation. |
| internal/domain/errors.go | Named errors used by validation/tests. |
| migrations/000001_create_ledger.sql | Tables, constraints, indexes, and triggers. |
| migrations/000002_create_owners.sql | Owner records and owner foreign keys. |
| internal/ledger | Account-code policy and owner/posting workflows. |
| internal/httpapi | Bearer-token-protected Phase 2 HTTP API. |
| tests/domain/types_test.go | Unit tests with no database. |
| tests/integration/postgres_test.go | Optional real-PostgreSQL test. |
| README.md | Setup and run commands. |
| docs/phase-1-parity.md | Rule-to-enforcement reference. |
| plan.md | Current and future scope. |

## Go module, packages, and dependencies

go.mod contains:

~~~go
module aurelialedger
~~~

That is the import prefix for this repository:

~~~go
import "aurelialedger/internal/config"
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
postgres://postgres:postgres@localhost:5432/aurelia_ledger?sslmode=disable
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

## Schema migrations: changing the database safely over time

A **schema migration** is a versioned SQL file that changes the structure of a
database: creating a table, adding a column, adding an index, and so on.

Phase 1 has one file:

~~~text
migrations/000001_create_ledger.sql
~~~

Later phases deliberately add files such as `000002_create_owners.sql` and
`000003_reservation_settlement.sql`. Once a migration has been applied to a
database, do not edit that file. Create a new migration instead. This preserves
a clear, reproducible history of how an older database reaches the current
schema.

### Why this project uses a Go migration command

For one brand-new local database, this direct command is simpler:

~~~bash
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -1 -f migrations/000001_create_ledger.sql
~~~

However, it only executes one file. It does not remember that the file ran, so
running it again usually fails because tables already exist. It also does not
choose and run later files in order.

Aurelia Ledger is planned to have several forward-only migrations plus other Go
operational commands (`cmd/reconcile` and `cmd/import`). Therefore it uses one
small Go command instead:

~~~bash
go run ./cmd/migrate up
~~~

This is not because Go makes SQL safer by itself. PostgreSQL still executes and
protects the SQL. The Go command provides a consistent, repeatable policy around
the SQL: discover files, decide which have already run, serialize concurrent
operators, and record successful work.

### End-to-end command flow

~~~text
DATABASE_URL
  -> cmd/migrate validates the single "up" argument
  -> config.Load reads DATABASE_URL
  -> postgres.Open creates a pool and verifies PostgreSQL is reachable
  -> LoadMigrations reads migrations/*.sql and sorts filenames
  -> ApplyMigrations uses one PostgreSQL connection
       -> acquire advisory lock
       -> create/check schema_migrations
       -> for every unapplied file:
            BEGIN
              execute that SQL file
              record its filename
            COMMIT
       -> release advisory lock
~~~

`cmd/migrate/main.go` is deliberately small. It rejects anything except `up`,
loads configuration, loads the migration files, opens the database, and calls
`ApplyMigrations`. The server never calls this command or migration package by
itself: applying schema changes is an explicit operator step before server
startup.

### Migration filenames and ordering

`LoadMigrations` reads regular files ending in `.sql`, then sorts their names
lexicographically (dictionary order):

~~~text
000001_create_ledger.sql
000002_create_owners.sql
000003_reservation_settlement.sql
~~~

The zero-padded numeric prefix is important. It makes filename order equal the
intended schema-change order. A non-SQL file is ignored; an empty migration
directory is an error rather than a silently successful no-op.

Each filename becomes a migration version. The command stores versions in this
PostgreSQL table:

~~~sql
CREATE TABLE IF NOT EXISTS schema_migrations (
    version TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
~~~

After the current file succeeds, a row conceptually looks like this:

~~~text
version                         applied_at
000001_create_ledger.sql        2026-08-12 10:30:00+00
~~~

On the next run, the command asks PostgreSQL whether each filename is already
present. A present filename is skipped. That makes `go run ./cmd/migrate up`
**idempotent**: it is safe to run repeatedly and converges on the same schema.

### Why the advisory lock matters

Two terminals, CI jobs, or deployment processes could try to migrate the same
database at the same time. Without coordination, both could observe that a file
is absent and both could try to apply it.

The migrator first asks PostgreSQL for an advisory lock. The first command gets
it; another command against the same database waits. Once the first finishes,
the waiting command obtains the lock, rechecks `schema_migrations`, sees the
recorded filename, and skips it.

~~~text
Terminal A                         Terminal B
----------                         ----------
gets lock
applies 000001
records 000001
releases lock                      gets lock
                                  sees 000001 recorded
                                  skips it
                                  releases lock
~~~

PostgreSQL advisory locks belong to one physical connection. That is why
`ApplyMigrations` gets a dedicated `*sql.Conn` from the pool and retains it from
`pg_advisory_lock` through `pg_advisory_unlock`. A plain `*sql.DB` represents a
pool and may use different connections for different calls, which would make a
session-level lock unreliable.

### One transaction per migration

For each missing file, the migrator performs:

~~~text
BEGIN
  execute migration SQL
  INSERT INTO schema_migrations(version) VALUES ('000001_create_ledger.sql')
COMMIT
~~~

The migration record is in the same transaction as the SQL. Therefore these two
outcomes cannot be separated:

~~~text
SQL succeeds and COMMIT succeeds -> schema changes and migration record both exist
SQL fails or COMMIT fails         -> PostgreSQL rolls back both
~~~

If `000002` fails after `000001` previously succeeded, `000001` remains safely
recorded. Fix `000002` and rerun the command; it skips `000001` and retries only
the missing file. This is why the runner uses one transaction *per file*, not
one transaction for the entire migration history.

### How migrations are tested

The opt-in PostgreSQL integration test now exercises the real migration code:

1. It drops the disposable test schema, including `schema_migrations`.
2. It loads and applies migrations once.
3. It runs the migrator a second time to prove rerunning works.
4. It verifies that `schema_migrations` has one row per discovered SQL file.

It only runs with a disposable database because it drops tables:

~~~bash
AURELIA_LEDGER_TEST_DATABASE_URL="$DATABASE_URL" go test ./tests/integration/...
~~~

Never point that variable at a database containing data you need to keep.

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
GOCACHE=/private/tmp/aurelia_ledger_gocache go test ./...
~~~

The domain tests need no database. The integration test is opt-in:

~~~bash
AURELIA_LEDGER_TEST_DATABASE_URL="$DATABASE_URL" go test ./tests/integration/...
~~~

It drops/recreates tables and functions, so its URL must point to an empty,
disposable database. Never point it at important data.

Start local PostgreSQL:

~~~bash
docker run --rm --name aurelia-ledger-postgres \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=aurelia_ledger \
  -p 5432:5432 postgres:16
~~~

Then, in another terminal:

~~~bash
cd /Users/smavani/dev/Aurelia-Ledger
export DATABASE_URL='postgres://postgres:postgres@localhost:5432/aurelia_ledger?sslmode=disable'
go run ./cmd/migrate up
go run ./cmd/server
curl http://localhost:8080/healthz
~~~

Run `go run ./cmd/migrate up` before every server startup. It is safe to rerun:
already-recorded migration files are skipped. The server does not automatically
apply migrations, so a successful `/healthz` response still proves only that
PostgreSQL is reachable.

## Core takeaways

~~~text
Go provides typed values, early validation, HTTP, and transaction orchestration.
PostgreSQL is the durable source of truth and enforces final data integrity.
Health checks prove connectivity, not ledger writes.
Headers describe events; entries record debit/credit lines.
Headers and entries are append-only.
PostgreSQL checks balance at COMMIT after it can see all entries.
The migrator applies ordered schema changes once, records them, and prevents
concurrent migration commands from racing.
~~~

## Phase 2: the owner, account, and journal model

Phase 2 introduces an important distinction: an **owner** is not an accounting
account.

~~~text
ledger_owners.id
  ├─ ledger_transactions.owner_id  = who the business event belongs to
  └─ ledger_accounts.owner_id      = who owns an owner wallet

ledger_accounts.id
  └─ ledger_entries.account_id     = which accounting account a posting changes
~~~

For example, a deposit for customer Alice can have this shape:

~~~text
Owner:                  Alice, ID 42
Transaction owner:      owner_id = 42
Wallet account:         code = wallet:42, account.owner_id = 42
Source account:         code = source:stripe, account.owner_id = NULL

Entries:
  debit  wallet:42       100  (owner's available balance increases)
  credit source:stripe   100  (the source side of the balanced event)
~~~

`ledger_owners` stores a business identity: `customer`, `team`, or `project`;
an external reference; a display name; metadata; and a cached balance. Its
database constraints reject unknown owner types, blank names/references, and
duplicate `(owner_type, external_ref)` pairs.

### Why `RETURNING` is used when creating an owner

The owner repository inserts only caller-supplied values:

~~~sql
INSERT INTO ledger_owners (owner_type, external_ref, display_name, metadata)
VALUES (...)
RETURNING id, cached_balance, created_at, updated_at
~~~

The returned `cached_balance` is initially the default `0`, but returning it
is still useful. PostgreSQL, not Go, produces the identity ID and timestamps.
Returning all database-produced values gives the API the exact stored record
without a second query, and remains correct if future defaults or triggers
change.

## Canonical account codes and source registration

There is deliberately no `account_role` column. `ledger_accounts.code` is the
unique business identifier of one account, and its canonical prefix establishes
the role:

| Code form | Meaning | `owner_id` |
| --- | --- | --- |
| `wallet:<owner-id>` | One owner's available-token wallet | Must equal that owner ID |
| `source:<registered-name>` | System source used for deposits | `NULL` |
| `sink:spend` | System sink used for spends | `NULL` |

The service validates this convention before any write. Thus a transaction for
owner 42 cannot use `wallet:99` merely because both rows exist.

### Account provisioning policy

Creation and locking are now separate operations.

~~~text
CreateOwner
  -> inserts the owner
  -> creates wallet:<owner-id> in the same transaction

Migration startup
  -> provisions source:other and sink:spend

POST /v1/register-sources
  -> creates a named system source, for example source:stripe

Deposit / spend / adjustment
  -> locks existing accounts only
  -> rejects a missing account
~~~

This matters for security and correctness. A client typo such as
`external_source="stirpe"` must not silently create `source:stirpe` and turn it
into a permanent ledger account. An authenticated operator first registers a
source:

~~~text
POST /v1/register-sources
{"name":"stripe","display_name":"Stripe","metadata":{}}
~~~

Source names are restricted to lowercase letters, digits, hyphens, and
underscores. A deposit that names an unregistered source returns `not_found`.

## Transactions, posting, and atomicity

`internal/ledger/service.go` is the application workflow layer. It owns the
business sequence; repositories only perform focused SQL operations.

~~~text
Deposit/Spend/Adjust
  -> build or accept postings
  -> validate request, metadata, idempotency key, and posting balance
  -> begin one SQL transaction
  -> lock owner and required existing accounts
  -> calculate signed account deltas
  -> reject an overspend if required
  -> insert immutable transaction header and entries
  -> update account.current_balance and owner.cached_balance
  -> commit
~~~

The transaction wrapper does not itself make data safe. It groups the work so
PostgreSQL can atomically commit all changes or roll all of them back. The
database also enforces foreign keys, positive amounts, append-only journal rows,
and deferred debit/credit balancing at commit.

### Why validation happens before SQL and again at the repository boundary

`post` validates early because it must know the postings are well formed before
it selects account codes, starts locks, or calculates balances. Early rejection
avoids unnecessary database work and brief blocking of valid requests.

`InsertEntries` also validates `transactionID`, the posting set, and that every
posting has a locked account in the supplied account map. This is intentional
defense in depth:

~~~text
Service validation     -> clear, early business/API error
Repository validation  -> safe persistence boundary if a future caller bypasses service
PostgreSQL constraints -> final durable guard even if application code is wrong
~~~

The small CPU cost of validating a short posting list twice is negligible
compared with the value of preventing an unsafe repository call.

### Idempotency

`external_source` and `external_id` form an optional pair. They are either both
present or both absent. Together they identify one external business event:

~~~text
stripe + invoice_123 = one payment notification
~~~

The database has a partial unique index on that pair. If the same notification
is retried, insertion fails with a unique violation, the service returns
`duplicate_transaction`, and the surrounding transaction rolls back. Balances
therefore do not change twice.

## Row locks and concurrent writes

Before changing a balance, the service locks the owner and all affected accounts
inside its SQL transaction.

~~~sql
SELECT ... FROM ledger_owners WHERE id = $1 FOR UPDATE;
SELECT ... FROM ledger_accounts WHERE code = $1 FOR UPDATE;
~~~

`FOR UPDATE` is a locking read. It is not a normal read lock: PostgreSQL lets
ordinary readers see a committed version using MVCC, but prevents a conflicting
writer or another locking reader from proceeding for that row until the current
transaction commits or rolls back.

The lock is acquired when PostgreSQL executes the `SELECT ... FOR UPDATE`. It
is released automatically by the transaction's `COMMIT` or `ROLLBACK`; Go does
not manually unlock it.

Example: two requests both try to spend 70 from a wallet with 100.

~~~text
Request A locks wallet:42, sees 100, writes new balance 30, commits.
Request B then obtains the lock, sees 30, calculates -40, and rolls back with
insufficient_funds.
~~~

Accounts are locked in ascending account-code order. If two adjustments name
the same accounts in opposite input order, both still request locks in the same
order. This avoids the classic deadlock pattern where each transaction holds
one account while waiting for the other.

### Why deterministic account ordering prevents a deadlock

A **deadlock** is a circular wait. It is not merely one request waiting for
another. For example, imagine two transactions that both need to change these
two accounts:

~~~text
source:stripe
wallet:42
~~~

Without one shared ordering rule, the client-provided posting order could make
the transactions take their locks in opposite directions:

~~~text
Transaction A                         Transaction B
-------------                         -------------
locks wallet:42
                                      locks source:stripe
tries to lock source:stripe; waits    tries to lock wallet:42; waits
~~~

A is waiting for B, while B is waiting for A. Neither transaction can reach
`COMMIT` and release its first lock. PostgreSQL detects this circular wait and
aborts one transaction with a deadlock error; that preserves database
correctness, but the rejected request must fail or be safely retried.

The existing implementation removes that circle by making the repository,
not the caller, choose the order. `LockExistingAccounts` in
`internal/database/postgres/ledger_repository.go` first deduplicates the
account codes, calls `sort.Strings(codes)`, and then executes one
`SELECT ... FOR UPDATE` per sorted code. Therefore both transactions above
must lock `source:stripe` before `wallet:42`, regardless of the order in which
the API client supplied their postings:

~~~text
Transaction A                         Transaction B
-------------                         -------------
locks source:stripe
                                      waits for source:stripe
locks wallet:42
COMMIT; releases both locks
                                      locks source:stripe
                                      locks wallet:42
                                      COMMIT
~~~

B still waits, but it holds no later account while waiting for the earlier
one. That is a queue, not a circle, so these account locks cannot deadlock
with each other. "Ascending" is not magic: descending order or sorted account
IDs would also work. The important rule is that **every path which locks the
same set of accounts uses exactly the same stable order**.

This is limited to the account locks handled by `LockExistingAccounts`. A
future workflow that locks additional resources (for example, a reservation
row) must define and follow a compatible order for those resources too;
otherwise it could introduce a new lock cycle.

## Foreign keys: existence versus semantic correctness

An FK from `ledger_transactions.owner_id` to `ledger_owners.id` proves only
that an owner ID exists. It cannot, by itself, prove that the transaction's
stored owner type matches the owner's actual type.

Bad direct SQL example:

~~~text
ledger_owners:       id=42, owner_type=customer
transaction attempt: owner_id=42, owner_type=team
~~~

The Phase 2 owner migration adds a unique owner pair `(id, owner_type)` and a
composite foreign key:

~~~text
(ledger_transactions.owner_id, ledger_transactions.owner_type)
  -> (ledger_owners.id, ledger_owners.owner_type)
~~~

There is no `(42, team)` owner pair, so PostgreSQL rejects the bad header even
if direct SQL bypasses the Go posting service. The service still performs its
own validation because an FK cannot verify that a transaction's postings use
the correct owner wallet.

## Repository interfaces and query methods

Both `*sql.DB` and `*sql.Tx` can run queries. Repository reads use narrow
interfaces so they can work either outside a transaction (`*sql.DB`) or inside
one (`*sql.Tx`) without duplicating methods.

~~~go
type singleRowQuerier interface {
    QueryRowContext(context.Context, string, ...any) *sql.Row
}

type queryExecutor interface {
    singleRowQuerier
    QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}
~~~

`QueryRowContext` is for an expected zero-or-one row. It returns `*sql.Row`,
which is read with `.Scan(...)`. `QueryContext` is for many rows. It returns
`*sql.Rows`, which is iterated with `rows.Next()`, scanned one row at a time,
and closed with `rows.Close()`.

The earlier runtime type assertion helper was removed. `queryExecutor` now
states the full compile-time requirement for history queries. This is clearer:
the Go compiler verifies that a supplied database value supports both one-row
and many-row queries.

## Cursor pagination

A cursor is a bookmark, not an authorization token. Transaction history is
ordered newest-first by `(created_at, id)`.

~~~text
First page, limit 2:
  10:05 / 105
  10:04 / 104

next_cursor represents: (10:04, 104)

Second page asks for rows older than that pair:
  10:03 / 103
  10:02 / 102
~~~

The SQL uses:

~~~sql
AND (created_at, id) < ($cursor_created_at, $cursor_id)
ORDER BY created_at DESC, id DESC
~~~

The ID is a tie-breaker when two transactions share a timestamp. Cursor
pagination is preferable to `OFFSET` for growing history because it avoids
walking past thousands of older rows and is less likely to duplicate/skip rows
when new transactions arrive during pagination. The cursor is Base64-encoded
JSON for transport; Base64 is not encryption.

## Phase 2 HTTP API and security

`/healthz` is public. Every `/v1/*` route requires:

~~~text
Authorization: Bearer <AURELIA_LEDGER_API_TOKEN>
~~~

The server refuses to start without `AURELIA_LEDGER_API_TOKEN`. The migration
command does not require it, because schema migration is not HTTP service
operation. Token comparison uses `crypto/subtle.ConstantTimeCompare`, avoiding
early-exit byte comparisons that can leak prefix information through timing.

The successful Phase 2 routes are:

~~~text
POST /v1/owners
POST /v1/register-sources
POST /v1/owners/{ownerID}/deposits
POST /v1/owners/{ownerID}/spends
POST /v1/owners/{ownerID}/adjustments
GET  /v1/owners/{ownerID}/balance
GET  /v1/owners/{ownerID}/transactions
GET  /v1/transactions/{transactionID}
POST /v1/owners/{ownerID}/reconcile
~~~

All errors have one stable JSON shape:

~~~json
{"error":{"code":"validation_error","message":"..."}}
~~~

Expected error classes are validation (`400`), unauthenticated (`401`), absent
owner/account/transaction (`404`), insufficient funds or duplicate transaction
(`409`), and unexpected internal failures (`500`).

## Reconciliation and cached projections

The append-only journal is authoritative. `ledger_accounts.current_balance` and
`ledger_owners.cached_balance` are fast projections derived from it.

Reconciliation recomputes the canonical wallet balance directly from entries:

~~~text
debit  amount -> add amount
credit amount -> subtract amount
~~~

It then updates both projections in one transaction. It does not create a new
journal transaction because no new business event occurred: it repairs derived
state that has drifted from existing history.

## What the Phase 2 tests prove

The opt-in PostgreSQL integration suite uses an empty disposable database. It
applies migrations twice to prove idempotency, then tests:

~~~text
- immutable journal rows and deferred balance checks
- automatic owner-wallet creation
- source registration and unregistered-source rejection
- deposits, duplicate idempotency, spends, and overspend rollback
- balanced adjustments, metadata, history, cursor pagination, and transaction reads
- reconciliation after deliberately corrupting cached projections
- composite owner-type foreign-key rejection
- simultaneous owner creation
- five concurrent deposits
- two competing overspends
- opposite posting input order with deterministic account locks
- every authenticated HTTP endpoint and auth failure
~~~

The tests run only when `AURELIA_LEDGER_TEST_DATABASE_URL` points to disposable
PostgreSQL. This is intentional: they drop/recreate tables and must never run
against valuable data.

## Phase 3: reservations, capture, and release

Phase 3 adds a **reservation lifecycle**. A reservation temporarily removes
tokens from an owner's spendable balance without yet treating them as consumed.
It has three operations:

~~~text
reserve  -> move available tokens into a held/reserved account
capture  -> consume some held tokens after work succeeds
release  -> return unused held tokens to the available account
~~~

For owner `42`, the implementation uses two owner-owned accounts:

~~~text
wallet:42              available tokens
wallet:42:reserved     tokens held for an in-progress operation
~~~

The exact `wallet:<ownerID>:reserved` convention is important. It keeps the
reserved account visibly tied to its owner and lets validation reject a caller
attempting to post to another owner's wallet, for example
`wallet:99:reserved` while acting for owner `42`.

### The accounting movements

Aurelia Ledger defines an account balance as:

~~~text
balance = sum(debits) - sum(credits)
~~~

Assume owner 42 has 100 available tokens and reserves 80:

~~~text
reserve 80
------------
credit wallet:42               80    available: 100 -> 20
debit  wallet:42:reserved      80    reserved:    0 -> 80
~~~

The owner's total represented tokens are still 100, but only 20 are currently
available for ordinary `Spend` operations. The reservation posting sets
`EnforcePositive: true` on the available-wallet credit, so a user cannot
reserve more tokens than are available.

If an external operation successfully uses 30 of the reservation, capture
writes another balanced journal transaction:

~~~text
capture 30
------------
credit wallet:42:reserved      30    reserved: 80 -> 50
debit  sink:consumed           30
~~~

`sink:consumed` is a system-owned account created by the Phase 3 migration.
It is deliberately separate from `sink:spend`, which records direct spend
operations. Separating them lets reporting distinguish immediate spends from
amounts consumed after a reservation.

If the remaining 50 is no longer needed, release writes:

~~~text
release 50
------------
credit wallet:42:reserved      50    reserved: 50 -> 0
debit  wallet:42               50    available: 20 -> 70
~~~

The completed reservation therefore has `captured=30`, `released=50`, and
`original=80`.

### Reservation projection versus journal history

The journal header and entry rows remain immutable. Phase 3 adds the mutable
`ledger_reservations` projection, one row for each reserve transaction. It
stores:

~~~text
reservation_transaction_id
owner_id
original_amount
captured_amount
released_amount
status: open or settled
~~~

The database checks enforce:

~~~text
captured_amount >= 0
released_amount >= 0
captured_amount + released_amount <= original_amount
status is settled exactly when captured_amount + released_amount = original_amount
~~~

For example, a second capture of 60 after a first capture of 60 from an
original reservation of 100 must fail: it would attempt to settle 120 tokens.
The service rejects it with `ErrReservationOverSettled`, and the database
constraint provides an additional backstop against invalid persisted state.

`reservation_transaction_id` is both the projection's primary key and a
foreign key to the original reserve transaction. Capture and release journal
headers also store that reserve transaction as their `parent_transaction_id`.
This creates a traceable chain:

~~~text
reserve transaction #100
  ├─ capture transaction #101
  └─ release transaction #102
~~~

`ON DELETE RESTRICT` prevents removal of a referenced owner or journal header.
This is appropriate for financial-like history: correct mistakes with a new
reversal or adjustment rather than deleting the original event.

### Required capture amount; optional release amount

`SettlementCommand.Amount` is a pointer:

~~~go
Amount *domain.Amount
~~~

The pointer lets Go distinguish omitted JSON from an explicit numeric value.

~~~text
nil                 field was omitted
&Amount(0)          caller supplied zero
&Amount(30)         caller supplied 30
~~~

Capture requires a supplied positive amount. This makes the amount consumed
explicit and prevents an accidental full capture:

~~~json
POST /v1/reservations/100/capture
{"amount":30,"metadata":{}}
~~~

An omitted capture amount returns `400 validation_error`.

Release accepts an omitted amount. It means **release the complete remaining
held amount**, not the amount already captured. With original 80 and captured
30, this request releases 50:

~~~json
POST /v1/reservations/100/release
{}
~~~

After a reservation is fully settled, further capture or release requests fail
instead of silently creating extra accounting entries.

### Transaction boundaries and concurrent settlement

Capture and release first execute:

~~~sql
SELECT ... FROM ledger_reservations
WHERE reservation_transaction_id = $1
FOR UPDATE;
~~~

`FOR UPDATE` takes a PostgreSQL row lock until the transaction commits or
rolls back. This serializes competing settlement attempts for one reservation.

~~~text
Reservation original amount: 100

Request A wants to capture 60
Request B wants to capture 60

A locks the reservation, captures 60, and commits.
B waits for A's lock, then sees only 40 remaining and fails.
~~~

The service then locks involved accounts through the existing canonical
ascending-account-code path. It always locks the available wallet even for a
capture, which does not directly post to it, because the cached available
balance is updated in that same transaction. Locking it avoids stale cached
balance writes racing with a spend, reserve, or release.

### Public HTTP operations and their authorization boundary

Reservations are not internal-only. Bearer-authenticated HTTP clients can use:

~~~text
POST /v1/owners/{ownerID}/reservations
POST /v1/reservations/{reservationID}/capture
POST /v1/reservations/{reservationID}/release
GET  /v1/reservations/{reservationID}
~~~

For example:

~~~bash
curl -X POST http://localhost:8080/v1/owners/42/reservations \
  -H "Authorization: Bearer $AURELIA_LEDGER_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"amount":80,"description":"provider hold","metadata":{}}'
~~~

The current API uses one local operator token. It authenticates the caller,
but it does **not** yet authorize a particular caller to act only for a
particular owner. Therefore every holder of that token can currently operate
on every owner ID. A user-facing deployment needs identity and ownership/role
checks before exposing these routes to untrusted users.

### `SpendWithOperation` is deliberately internal-only

`SpendWithOperation` has no HTTP endpoint. It is a Go method intended for an
in-process provider adapter:

~~~go
type ExternalWork interface {
    Execute(context.Context) error
}
~~~

Its sequence is:

~~~text
1. reserve and commit the local hold
2. run ExternalWork outside the database transaction
3. capture the full reservation if ExternalWork succeeds
4. release the remaining hold if ExternalWork fails
~~~

The external call must happen outside the SQL transaction. A provider request
may be slow, may time out, and cannot be rolled back by PostgreSQL. Holding
database row locks while waiting for a network call would reduce concurrency
and still would not make the two systems atomic.

If the external call fails and release also fails, `errors.Join(workErr,
releaseErr)` preserves both errors. The provider error explains why the work
failed; the release error tells an operator that held funds may need recovery.

The tests use a small function adapter rather than a real provider:

~~~go
type externalWorkFunc func(context.Context) error

func (f externalWorkFunc) Execute(ctx context.Context) error { return f(ctx) }

success := externalWorkFunc(func(context.Context) error { return nil })
failure := externalWorkFunc(func(context.Context) error {
    return errors.New("provider failed")
})
~~~

The success case proves that the reservation is captured. The failure case
proves that the reservation is released and the provider error is returned.
They test orchestration, not an actual network provider.

### Relationship to real payment systems

Reserve/capture/release is one common form of a wider pattern:

~~~text
hold or commit local funds
-> perform checks or external work
-> finalize, release, reverse, or compensate
~~~

Card payments often resemble `authorize -> capture -> void/reversal`.
Marketplace or usage-based systems often resemble this application's
`reserve -> capture/release` flow. In contrast, an internal transfer between
two wallets controlled by one database can usually be a single atomic debit
and credit transaction. A bank transfer normally has more states, such as
`initiated -> screened -> accepted/rejected -> settled/returned`, because
multiple banks and payment rails exchange messages independently.

A local reservation means only that *this application's* ledger will not
spend those funds twice. It is not proof that an external bank, provider, or
payment rail accepted the operation. Production cross-system payment flows
also need provider operation IDs, retry-safe idempotency, webhooks or polling,
expiry handling, reconciliation, and compensating journal entries. A
transactional outbox is useful when the application promises reliable
delivery of downstream events, but it is intentionally not part of this Phase
3 implementation.

### Phase 3 test coverage and execution boundary

The opt-in PostgreSQL integration tests now cover:

~~~text
- reserved-account creation with wallet:<ownerID>:reserved
- insufficient funds after a reservation
- required capture amount
- partial capture and default release of the remaining hold
- readback of reservation state and rejection after settlement
- release followed by capture
- concurrent capture attempts against the same reservation
- SpendWithOperation success and callback-failure release
- public reserve/capture/release/get HTTP routes and validation errors
~~~

The suite remains opt-in because it drops and recreates database tables:

~~~bash
AURELIA_LEDGER_TEST_DATABASE_URL='postgres://.../disposable_db' \
  GOCACHE=/private/tmp/aurelia_ledger_gocache go test ./tests/integration -v
~~~

Never point this variable at a database containing valuable data.
