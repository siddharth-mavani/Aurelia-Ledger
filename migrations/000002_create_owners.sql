-- Phase 2 owner model. This migration only adds nullable columns to existing
-- tables so it remains safe for databases that already contain Phase 1 data.
CREATE TABLE ledger_owners (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_type TEXT NOT NULL,
    external_ref TEXT NOT NULL,
    display_name TEXT NOT NULL,
    cached_balance BIGINT NOT NULL DEFAULT 0,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ledger_owners_type CHECK (owner_type IN ('customer', 'team', 'project')),
    CONSTRAINT ledger_owners_external_ref_not_empty CHECK (length(btrim(external_ref)) > 0),
    CONSTRAINT ledger_owners_display_name_not_empty CHECK (length(btrim(display_name)) > 0),
    CONSTRAINT ledger_owners_metadata_object CHECK (jsonb_typeof(metadata) = 'object'),
    CONSTRAINT ledger_owners_type_external_ref_key UNIQUE (owner_type, external_ref),
    CONSTRAINT ledger_owners_id_owner_type_key UNIQUE (id, owner_type)
);

ALTER TABLE ledger_accounts
    ADD COLUMN owner_id BIGINT REFERENCES ledger_owners(id) ON DELETE RESTRICT;
ALTER TABLE ledger_transactions
    ADD CONSTRAINT ledger_transactions_owner_type_id_fkey
    FOREIGN KEY (owner_id, owner_type)
    REFERENCES ledger_owners(id, owner_type) ON DELETE RESTRICT;

CREATE INDEX ledger_accounts_owner_id_idx ON ledger_accounts(owner_id);
CREATE INDEX ledger_transactions_owner_id_created_at_idx
    ON ledger_transactions(owner_id, created_at DESC, id DESC);

INSERT INTO ledger_accounts (code, name, owner_id, metadata)
VALUES
    ('source:other', 'Other Token Source', NULL, '{}'::jsonb),
    ('sink:spend', 'Token Spend Sink', NULL, '{}'::jsonb)
ON CONFLICT (code) DO NOTHING;
