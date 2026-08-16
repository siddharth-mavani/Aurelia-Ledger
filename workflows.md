# Aurelia Ledger Workflows

This guide explains Aurelia Ledger through the operations a client performs.
It is a local Go and PostgreSQL service for tracking token balances. A displayed
wallet balance is a fast projection; the permanent explanation of each change
is an immutable, balanced double-entry journal transaction.

All `/v1/*` examples require a bearer token:

```bash
export API='http://localhost:8080'
export AUTH="Authorization: Bearer $AURELIA_LEDGER_API_TOKEN"
```

Amounts are positive integer token base units. The examples use owner `1` once
it has been created. A code path describes the request's path through the
current implementation; it is not a claim that an external provider action is
part of the same database transaction.

## The moving pieces

```text
Client request
  -> HTTP API: authentication, routing, JSON decoding, HTTP error mapping
  -> Ledger service: business workflow and one SQL transaction
  -> PostgreSQL repositories: locks, journal rows, and balance projections
```

Each owner has two accounts:

```text
wallet:1             tokens currently available to spend
wallet:1:reserved    tokens held by an open reservation
```

System accounts provide the other side of a transaction: `source:<name>` for
deposits, `sink:spend` for direct spending, and `sink:consumed` for captured
reservations. In journal terminology, an account's balance is:

```text
sum(debits) - sum(credits)
```

## 1. Set up an owner and a trusted token source

**User goal:** create Alice's wallet and allow deposits from Stripe.

```bash
curl -X POST "$API/v1/owners" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"type":"customer","external_ref":"alice@example.com","display_name":"Alice","metadata":{}}'

curl -X POST "$API/v1/register-sources" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"stripe","display_name":"Stripe","metadata":{}}'
```

The first call creates an owner record and two owner-owned accounts,
`wallet:1` and `wallet:1:reserved`, in one SQL transaction. The second call
creates the system-owned `source:stripe` account. This explicit registration
means a deposit cannot silently introduce a misspelled or untrusted source.

```text
POST /v1/owners
  -> API.createOwner
  -> Service.CreateOwner
  -> owner repository INSERT + ledger CreateAccount twice
  -> commit

POST /v1/register-sources
  -> API.registerSource
  -> Service.RegisterSource
  -> ledger CreateAccount(source:stripe)
  -> commit
```

An owner type must be `customer`, `team`, or `project`; an external reference
and display name are required. A duplicate `(owner_type, external_ref)` or a
duplicate source code is rejected rather than creating a second identity.

## 2. Deposit tokens: the normal purchase case

**User goal:** add 100 purchased tokens to Alice's wallet exactly once.

```bash
curl -X POST "$API/v1/owners/1/deposits" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"amount":100,"description":"Token purchase","external_source":"stripe","external_id":"invoice_123","metadata":{}}'
```

**What Alice sees:** the API returns `201 Created` with a transaction ID and
an available balance of `100`. A later `GET /v1/owners/1/balance` returns
`100`.

**What changed conceptually:**

| Account | Entry | Effect on balance |
| --- | --- | ---: |
| `source:stripe` | credit 100 | -100 |
| `wallet:1` | debit 100 | +100 |

The source account records where tokens originated; Alice's wallet holds
tokens she can spend. The equal debit and credit totals make this a balanced
double-entry transaction.

```text
HTTP request
  -> API authenticates the bearer token and parses JSON
  -> Deposit command validates amount and idempotency fields
  -> Service.Deposit builds the two postings
  -> Service.post starts one PostgreSQL transaction and locks owner 1
  -> postInTransaction locks wallet:1 and source:stripe
  -> inserts immutable transaction header and entries
  -> updates current_balance and owner cached_balance
  -> commit
```

The relevant implementation is `internal/httpapi/api.go` (`deposit`),
`internal/ledger/service.go` (`Deposit`, `post`, and `postInTransaction`), and
`internal/database/postgres/ledger_repository.go`.

## 3. A retried purchase: idempotency prevents a double credit

**Situation:** Stripe times out after submitting `invoice_123`, so the client
retries the exact request from workflow 2.

The first request can create the deposit. The retry attempts to insert another
transaction with the same `(external_source, external_id)` pair and PostgreSQL
rejects it through the partial unique index on `ledger_transactions`. The API
returns `409` with `duplicate_transaction`; it does not add another 100
tokens.

```text
first request:  source:stripe -> wallet:1     balance becomes 100
same retry:     duplicate transaction         balance stays 100
```

Both parts of the key must be present together. Sending only
`external_source` or only `external_id` is invalid input and returns `400`.
The key is scoped by source, so `stripe/invoice_123` and
`paypal/invoice_123` are distinct events.

## 4. Spend tokens: happy path, negative amount, and insufficient funds

**Happy path:** Alice has 100 available tokens and spends 25.

```bash
curl -X POST "$API/v1/owners/1/spends" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"amount":25,"description":"Generate report","external_source":"app","external_id":"job_456","metadata":{}}'
```

| Account | Entry | Balance after the operation |
| --- | --- | ---: |
| `wallet:1` | credit 25 | 75 |
| `sink:spend` | debit 25 | 25 |

`Service.Spend` marks the wallet posting with `EnforcePositive`. After locking
the account row, `postInTransaction` calculates the new signed balance before
it writes the journal rows. Therefore a request to spend 80 when the wallet
contains 75 returns `409` and changes nothing.

Negative amounts and zero are not a way to bypass this rule. `domain.Amount`
requires `amount > 0`, so a request such as `{"amount":-25,...}` returns
`400` before the SQL transaction begins. The database independently has an
`amount > 0` check on every persisted journal entry.

## 5. Two concurrent spends against one wallet

**Situation:** Alice has 100 tokens. Two workers concurrently submit distinct
spend requests for 70 tokens each.

```text
Worker A: spend 70       Worker B: spend 70
      |                         |
      +-- both target wallet:1 -+
                 |
             SELECT ... FOR UPDATE
                 |
       one request locks and commits first
                 |
       other request reads the new balance (30)
                 |
       first succeeds; second gets insufficient funds
```

The final available balance is `30`, not `-40`, and only one spend journal
transaction exists. PostgreSQL row locks serialize changes to the same account.
`LedgerRepository.LockExistingAccounts` locks all needed accounts in sorted
account-code order, which also avoids the usual circular lock ordering when a
workflow needs more than one account. It does not make a request instant: the
second request can wait while the first is in progress.

Use a distinct idempotency key per business event. Sending the *same* key from
both workers is a retry/duplicate case instead: at most one transaction is
inserted, and the other gets a duplicate conflict.

## 6. Reserve, capture part, then release the remainder

**User goal:** hold tokens before provider work, consume the part actually
used, and return the rest to Alice.

Starting from 100 available tokens:

```bash
# Hold 80. The response includes reservation_transaction_id; call it 10 here.
curl -X POST "$API/v1/owners/1/reservations" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"amount":80,"description":"Provider hold","external_source":"provider","external_id":"hold_123","metadata":{}}'

# Consume 30 of the held amount.
curl -X POST "$API/v1/reservations/10/capture" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"amount":30,"description":"Provider completed work","external_source":"provider","external_id":"capture_123","metadata":{}}'

# No amount means release every remaining held token: 50.
curl -X POST "$API/v1/reservations/10/release" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{}'
```

The movements are:

```text
reserve 80:  wallet:1 (100 -> 20)  -> wallet:1:reserved (0 -> 80)
capture 30:  reserved (80 -> 50)   -> sink:consumed
release 50:  reserved (50 -> 0)    -> wallet:1 (20 -> 70)
```

The reservation projection records `original_amount=80`, `captured_amount=30`,
`released_amount=50`, and `status=settled`. Every reserve, capture, and release
also has its own immutable journal transaction; capture and release reference
the original reserve transaction as their parent.

`Reserve` creates the reserve journal transaction and reservation projection
inside one transaction. `Capture` and `Release` use `GetForUpdate` to lock the
reservation row, calculate the remaining amount, post the corresponding
journal entries, and update the projection before committing.

## 7. Concurrent or invalid reservation settlement

**Situation:** reservation 10 has 50 tokens remaining. Two callers each try
to capture 30 at nearly the same time.

The first caller locks the reservation and may capture 30, leaving 20. The
second caller waits for that lock, then recalculates remaining amount as 20;
its capture of 30 is rejected. It cannot cause the reservation to consume 60.

```text
captured_amount + released_amount <= original_amount
```

The same condition exists in three layers: the service calculation, the
repository check, and PostgreSQL table constraints. Once captured plus released
equals the original amount, the status becomes `settled`; future capture or
release requests are rejected.

Other settlement cases:

- Capture requires an explicit, positive amount. Omitting it or sending zero
  is a `400` validation error.
- Release may omit `amount`; that intentionally means “release all remaining
  tokens.” A supplied amount must be positive and no greater than remaining.
- A missing reservation returns `404`.

## 8. Make a correction with an explicit adjustment

**User goal:** record a correction without rewriting history.

An adjustment accepts the exact balanced postings supplied by the operator.
For example, to correct Alice's available balance upward by 10 using the
built-in source account:

```bash
curl -X POST "$API/v1/owners/1/adjustments" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{
    "description":"Manual correction for duplicate provider reversal",
    "metadata":{},
    "postings":[
      {"account_code":"wallet:1","account_name":"Alice wallet","side":"debit","amount":10,"metadata":{}},
      {"account_code":"source:other","account_name":"Other source","side":"credit","amount":10,"metadata":{}}
    ]
  }'
```

This makes a new `adjustment` journal transaction; it does not update or
delete the original transaction. The service checks that postings are nonempty,
all account fields and sides are valid, all amounts are positive, and debit
total equals credit total. PostgreSQL checks the resulting transaction again
at commit through deferred constraint triggers.

An unbalanced adjustment, such as debit 10 and credit 9, returns `400` and no
journal rows commit. Adjustments intentionally differ from normal `spend` and
`reserve` operations: their caller-supplied postings are not automatically
marked with the available-funds rule, because corrections sometimes represent
an accounting repair. They should be performed only by a trusted local
operator; the current bearer token authenticates the operator but does not
provide per-owner authorization.

## 9. Read history and reconcile a cached balance

**User goal:** inspect what happened to Alice's tokens, then repair a derived
balance if it ever diverges from the journal.

```bash
curl -H "$AUTH" "$API/v1/owners/1/transactions?limit=50"
curl -H "$AUTH" "$API/v1/transactions/12"
curl -X POST -H "$AUTH" "$API/v1/owners/1/reconcile"
```

Owner history is ordered newest first and uses an opaque `next_cursor` for the
next page. A transaction lookup returns the immutable header plus its entries.

Reconciliation locks the owner and available wallet, recomputes the wallet
balance by summing its debit entries minus credit entries, then updates the
mutable `ledger_accounts.current_balance` and
`ledger_owners.cached_balance` projections in one transaction. It does not
create a compensating journal transaction, alter the journal, or change the
reserved balance. In ordinary operation it reports `repaired: false`; it is a
repair path for a derived projection, not the normal way to obtain a balance.

## 10. Internal helper: reserve around external work

`Service.SpendWithOperation` is a Go-level helper rather than an HTTP route.
It performs this sequence:

```text
reserve locally and commit
  -> call ExternalWork.Execute outside PostgreSQL
  -> capture full reservation when it succeeds
     OR release the reservation when it fails
```

Running the external callback outside the SQL transaction is deliberate: a
database transaction should not remain open while waiting on a provider.
However, there is no distributed transaction here. If the provider succeeds
but the process crashes before capture, the reservation remains open and needs
recovery. Likewise, a failed release after provider failure returns both errors
to the caller. A production system that promises reliable downstream delivery
would need an additional recovery/outbox design and idempotent provider calls.

## How the data stays trustworthy

The workflows rely on complementary mechanisms rather than one validation
function:

| Concern | Application behavior | PostgreSQL backstop |
| --- | --- | --- |
| Valid request | API rejects unknown JSON fields; domain validates input | Column and JSON-object checks |
| Balanced journal | `ValidatePostings` runs before persistence | Deferred trigger checks entry count and debit/credit totals at commit |
| Durable all-or-nothing operation | Service groups each workflow in `WithinTransaction` | Commit applies all rows; rollback applies none |
| No duplicate external event | Service provides the key | Unique partial index on source and external ID |
| Concurrent balance changes | Account/reservation rows are locked before calculation | `FOR UPDATE` holds the row lock until commit or rollback |
| Historical auditability | Workflows create new transactions for corrections | Triggers reject updates and deletes of journal headers and entries |

The service is intentionally local-development scoped. `GET /healthz` only
checks PostgreSQL reachability, not migration state or posting correctness.
All API routes except that health check use one shared bearer token, so the
service should not be treated as a multi-user authorization system.
