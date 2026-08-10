-- Phase 1 canonical PostgreSQL schema. Apply this file in one transaction.
CREATE TABLE ledger_accounts (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    current_balance BIGINT NOT NULL DEFAULT 0,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ledger_accounts_metadata_object CHECK (jsonb_typeof(metadata) = 'object')
);

CREATE TABLE ledger_transactions (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_type TEXT NOT NULL,
    description TEXT NOT NULL,
    owner_type TEXT,
    owner_id BIGINT,
    parent_transaction_id BIGINT REFERENCES ledger_transactions(id) ON DELETE RESTRICT,
    external_source TEXT,
    external_id TEXT,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ledger_transactions_type CHECK (transaction_type IN ('deposit', 'spend', 'reserve', 'capture', 'release', 'adjustment')),
    CONSTRAINT ledger_transactions_owner_pair CHECK ((owner_type IS NULL AND owner_id IS NULL) OR (owner_type IS NOT NULL AND owner_id IS NOT NULL)),
    CONSTRAINT ledger_transactions_external_pair CHECK ((external_source IS NULL AND external_id IS NULL) OR (external_source IS NOT NULL AND external_id IS NOT NULL)),
    CONSTRAINT ledger_transactions_metadata_object CHECK (jsonb_typeof(metadata) = 'object')
);

CREATE UNIQUE INDEX ledger_transactions_external_source_id_key
    ON ledger_transactions(external_source, external_id) WHERE external_source IS NOT NULL;
CREATE INDEX ledger_transactions_owner_created_at_idx ON ledger_transactions(owner_type, owner_id, created_at DESC);
CREATE INDEX ledger_transactions_type_created_at_idx ON ledger_transactions(transaction_type, created_at DESC);
CREATE INDEX ledger_transactions_parent_idx ON ledger_transactions(parent_transaction_id);

CREATE TABLE ledger_entries (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES ledger_accounts(id) ON DELETE RESTRICT,
    transaction_id BIGINT NOT NULL REFERENCES ledger_transactions(id) ON DELETE RESTRICT,
    entry_type TEXT NOT NULL,
    amount BIGINT NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ledger_entries_positive_amount CHECK (amount > 0),
    CONSTRAINT ledger_entries_entry_type CHECK (entry_type IN ('debit', 'credit')),
    CONSTRAINT ledger_entries_metadata_object CHECK (jsonb_typeof(metadata) = 'object')
);

CREATE INDEX ledger_entries_account_created_at_idx ON ledger_entries(account_id, created_at DESC);
CREATE INDEX ledger_entries_transaction_type_idx ON ledger_entries(transaction_id, entry_type);

-- Financial rows are append-only. Account cached balances remain mutable by Phase 2 postings.
CREATE FUNCTION ledger_reject_journal_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger journal rows are immutable' USING ERRCODE = '55000';
END;
$$;
CREATE TRIGGER ledger_transactions_immutable BEFORE UPDATE OR DELETE ON ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_journal_mutation();
CREATE TRIGGER ledger_entries_immutable BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_journal_mutation();

-- Deferred checks run at commit, after a header and all its entries have been inserted.
CREATE FUNCTION ledger_assert_transaction_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    checked_transaction_id BIGINT;
    debit_total BIGINT;
    credit_total BIGINT;
    entry_count BIGINT;
BEGIN
    IF TG_TABLE_NAME = 'ledger_transactions' THEN
        checked_transaction_id := NEW.id;
    ELSE
        checked_transaction_id := NEW.transaction_id;
    END IF;

    SELECT count(*),
           COALESCE(sum(amount) FILTER (WHERE entry_type = 'debit'), 0),
           COALESCE(sum(amount) FILTER (WHERE entry_type = 'credit'), 0)
      INTO entry_count, debit_total, credit_total
      FROM ledger_entries WHERE transaction_id = checked_transaction_id;

    IF entry_count = 0 OR debit_total <> credit_total THEN
        RAISE EXCEPTION 'ledger transaction % is not balanced', checked_transaction_id USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER ledger_transactions_must_balance
    AFTER INSERT ON ledger_transactions DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_assert_transaction_balanced();
CREATE CONSTRAINT TRIGGER ledger_entries_must_balance
    AFTER INSERT ON ledger_entries DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_assert_transaction_balanced();
