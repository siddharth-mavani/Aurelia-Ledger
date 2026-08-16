-- Phase 3 reservation projection. The journal remains authoritative; this
-- table is a mutable, transactionally maintained settlement projection.
CREATE TABLE ledger_reservations (
    reservation_transaction_id BIGINT PRIMARY KEY REFERENCES ledger_transactions(id) ON DELETE RESTRICT,
    owner_id BIGINT NOT NULL REFERENCES ledger_owners(id) ON DELETE RESTRICT,
    original_amount BIGINT NOT NULL,
    captured_amount BIGINT NOT NULL DEFAULT 0,
    released_amount BIGINT NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'open',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ledger_reservations_original_positive CHECK (original_amount > 0),
    CONSTRAINT ledger_reservations_totals_non_negative CHECK (captured_amount >= 0 AND released_amount >= 0),
    CONSTRAINT ledger_reservations_totals_within_original CHECK (captured_amount + released_amount <= original_amount),
    CONSTRAINT ledger_reservations_status_valid CHECK (status IN ('open', 'settled')),
    CONSTRAINT ledger_reservations_status_matches_totals CHECK (
        (status = 'settled') = (captured_amount + released_amount = original_amount)
    )
);

CREATE INDEX ledger_reservations_owner_id_idx ON ledger_reservations(owner_id);

-- Capture postings are deliberately distinct from direct spends.
INSERT INTO ledger_accounts (code, name, owner_id, metadata)
VALUES ('sink:consumed', 'Consumed Reservation Sink', NULL, '{}'::jsonb)
ON CONFLICT (code) DO NOTHING;
