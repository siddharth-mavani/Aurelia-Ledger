# Data integrity rules

| Rule | Enforcement |
| --- | --- |
| Account code is unique | `ledger_accounts.code UNIQUE` |
| Account cache is signed integer | `current_balance BIGINT NOT NULL DEFAULT 0` |
| Transaction header stores owner, parent, external key, metadata | explicit owner pair and restrictive parent FK |
| External source/ID occur together and are unique by source | paired-value check and partial unique index |
| Entry amounts are positive integers | `amount > 0` check and `domain.Amount.Validate` |
| Entries are debit or credit | entry-type check and `EntrySide` |
| Transaction types are fixed | transaction-type check and `TransactionType` |
| Debits equal credits | `ValidatePostings` plus deferred PostgreSQL constraint trigger |
| Ledger history is immutable | update/delete rejection triggers on headers and entries |
| JSON metadata defaults to an object | JSONB defaults and object checks |

Reservation parent links use restrictive deletion to protect the audit trail.
