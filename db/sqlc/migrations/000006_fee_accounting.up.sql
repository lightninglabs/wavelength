-- Client-side fee accounting tables using double-entry
-- bookkeeping, mirroring the server-side schema.

-- Account type enum table.
CREATE TABLE IF NOT EXISTS account_types (
    account_type TEXT PRIMARY KEY
);

INSERT INTO account_types (account_type) VALUES
    ('asset'),
    ('liability'),
    ('revenue'),
    ('expense')
ON CONFLICT DO NOTHING;

-- Ledger event type enum table.
CREATE TABLE IF NOT EXISTS ledger_event_types (
    event_type TEXT PRIMARY KEY
);

-- Client-side event types track fees paid by the user.
INSERT INTO ledger_event_types (event_type) VALUES
    ('boarding_fee_paid'),
    ('refresh_fee_paid'),
    ('onchain_fee_paid'),
    ('vtxo_received'),
    ('vtxo_sent')
ON CONFLICT DO NOTHING;

-- Chart of accounts from the client's perspective.
CREATE TABLE IF NOT EXISTS accounts (
    account_id TEXT PRIMARY KEY,
    account_name TEXT NOT NULL,
    account_type TEXT NOT NULL
        REFERENCES account_types(account_type)
);

INSERT INTO accounts (account_id, account_name, account_type) VALUES
    ('wallet_balance', 'Wallet Balance', 'asset'),
    ('vtxo_balance',   'VTXO Balance',   'asset'),
    ('fees_paid',      'Fees Paid',          'expense'),  -- Ark protocol fees to operator
    ('onchain_fees',   'On-Chain Fees Paid', 'expense'),  -- L1 chain/miner fees
    ('transfers_in',   'Transfers In',       'revenue'),  -- counterparty side of received VTXOs
    ('transfers_out',  'Transfers Out',      'expense')   -- counterparty side of sent VTXOs
ON CONFLICT DO NOTHING;

-- Double-entry ledger for client fee tracking.
CREATE TABLE IF NOT EXISTS ledger_entries (
    entry_id INTEGER PRIMARY KEY,

    debit_account TEXT NOT NULL
        REFERENCES accounts(account_id),

    credit_account TEXT NOT NULL
        REFERENCES accounts(account_id),

    -- amount_sat is the entry amount in satoshis.
    amount_sat BIGINT NOT NULL CHECK (amount_sat > 0),

    -- round_id optionally links this entry to a round.
    round_id BLOB,

    -- event_type classifies the entry.
    event_type TEXT NOT NULL
        REFERENCES ledger_event_types(event_type),

    -- description is a human-readable note.
    description TEXT NOT NULL,

    -- created_at is the Unix timestamp.
    created_at BIGINT NOT NULL,

    -- Debit and credit must target different accounts.
    CHECK (debit_account != credit_account)
);

CREATE INDEX IF NOT EXISTS idx_client_ledger_created
    ON ledger_entries(created_at DESC);

CREATE INDEX IF NOT EXISTS idx_client_ledger_event_type
    ON ledger_entries(event_type);

CREATE INDEX IF NOT EXISTS idx_client_ledger_round
    ON ledger_entries(round_id);

CREATE INDEX IF NOT EXISTS idx_client_ledger_debit
    ON ledger_entries(debit_account);

CREATE INDEX IF NOT EXISTS idx_client_ledger_credit
    ON ledger_entries(credit_account);

-- Prevent duplicate entries for the same round, event, and account pair.
-- NOTE: Entries with NULL round_id (e.g. onchain_fee_paid) are excluded from
-- this constraint intentionally. When on-chain fee events gain a natural dedup
-- key (txid/outpoint), a nullable idempotency_key column and a second partial
-- unique index can close this gap.
CREATE UNIQUE INDEX IF NOT EXISTS idx_client_ledger_idempotent
    ON ledger_entries(round_id, event_type, debit_account, credit_account)
    WHERE round_id IS NOT NULL;
