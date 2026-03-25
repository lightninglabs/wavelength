-- Accounting tables for operator fee tracking using double-entry
-- bookkeeping.

-- Account type enum table. Each row defines a valid account
-- classification for reporting.
CREATE TABLE IF NOT EXISTS account_types (
    account_type TEXT PRIMARY KEY
);

-- Populate account types.
INSERT INTO account_types (account_type) VALUES
    ('asset'),
    ('liability'),
    ('revenue'),
    ('expense')
ON CONFLICT DO NOTHING;

-- Ledger event type enum table. Each row defines a valid event
-- classification for ledger entries.
CREATE TABLE IF NOT EXISTS ledger_event_types (
    event_type TEXT PRIMARY KEY
);

-- Populate ledger event types. These fall into two groups.
--
-- (1) Fee-model events — correspond 1:1 to the event table in
-- docs/fee-model.md "Double-Entry Accounting" and are fine-grained
-- so the Admin RPC ListFeeEvents can filter by exact event without
-- parsing the description field.
--
-- (2) Wallet / OOR tracking events — extensions beyond the fee-model
-- spec that the operator uses for treasury reconciliation with the
-- on-chain wallet and for off-chain OOR transfer accounting. These
-- are intentionally co-located with the fee-model events so the
-- ledger carries a single unified event taxonomy; downstream
-- callers can filter by event_type regardless of origin.
INSERT INTO ledger_event_types (event_type) VALUES
    -- Fee-model events (docs/fee-model.md).
    ('boarding_deposit'),
    ('boarding_fee'),
    ('refresh_forfeit'),
    ('refresh_new_vtxo'),
    ('refresh_fee'),
    ('offboard'),
    ('mining_fee'),
    ('round_sweep'),
    ('capital_committed'),
    -- Wallet / OOR tracking events (operator treasury reconciliation).
    ('oor_transfer'),
    ('wallet_deposit'),
    ('wallet_spend'),
    ('sweep_return'),
    ('round_funding')
ON CONFLICT DO NOTHING;

-- Chart of accounts. Each account represents a logical bucket
-- in the operator's balance sheet. The seeded set mirrors the
-- chart of accounts in docs/fee-model.md so that
-- "operator equity = assets - liabilities" is computable
-- directly from the ledger.
CREATE TABLE IF NOT EXISTS accounts (
    -- account_id is the short mnemonic for the account (e.g.
    -- 'treasury_wallet').
    account_id TEXT PRIMARY KEY,

    -- account_name is the human-readable label.
    account_name TEXT NOT NULL,

    -- account_type classifies the account for reporting.
    account_type TEXT NOT NULL
        REFERENCES account_types(account_type)
);

-- Seed the chart of accounts with the operator's core accounts.
INSERT INTO accounts (account_id, account_name, account_type) VALUES
    ('treasury_wallet', 'Treasury Wallet', 'asset'),
    ('deployed_capital', 'Deployed Capital', 'asset'),
    ('user_vtxo_claims', 'User VTXO Claims', 'liability'),
    ('operator_revenue', 'Operator Fee Revenue', 'revenue'),
    ('mining_fees', 'Mining Fee Expense', 'expense')
ON CONFLICT DO NOTHING;

-- Double-entry ledger. Every financial event is recorded as a
-- pair of debit and credit entries that must balance.
CREATE TABLE IF NOT EXISTS ledger_entries (
    -- entry_id is the monotonically increasing primary key.
    entry_id INTEGER PRIMARY KEY,

    -- debit_account is the account being debited.
    debit_account TEXT NOT NULL
        REFERENCES accounts(account_id),

    -- credit_account is the account being credited.
    credit_account TEXT NOT NULL
        REFERENCES accounts(account_id),

    -- amount_sat is the transaction amount in satoshis. Must
    -- be strictly positive — zero-amount entries are rejected at
    -- the schema layer because they represent no economic event
    -- and would pollute audit counts.
    amount_sat BIGINT NOT NULL CHECK (amount_sat > 0),

    -- round_id optionally links this entry to a specific
    -- round.
    round_id BLOB,

    -- event_type classifies the ledger entry for filtering.
    event_type TEXT NOT NULL
        REFERENCES ledger_event_types(event_type),

    -- description is a human-readable note about the entry.
    description TEXT NOT NULL,

    -- created_at is the Unix timestamp when this entry was
    -- recorded.
    created_at BIGINT NOT NULL,

    -- A self-transfer (debit_account = credit_account) is a
    -- silent no-op: the +amount and -amount contributions to the
    -- same account cancel in any balance aggregation, so even the
    -- sum-to-zero invariant cannot detect it. Reject at the schema
    -- layer so a buggy caller cannot pollute the audit log.
    CHECK (debit_account <> credit_account)
);

CREATE INDEX IF NOT EXISTS idx_ledger_round
    ON ledger_entries(round_id);

CREATE INDEX IF NOT EXISTS idx_ledger_created
    ON ledger_entries(created_at DESC);

CREATE INDEX IF NOT EXISTS idx_ledger_debit
    ON ledger_entries(debit_account);

CREATE INDEX IF NOT EXISTS idx_ledger_credit
    ON ledger_entries(credit_account);

CREATE INDEX IF NOT EXISTS idx_ledger_event_type
    ON ledger_entries(event_type);

-- Fee schedule history. Each row records a fee schedule change
-- for audit purposes.
CREATE TABLE IF NOT EXISTS fee_schedule_history (
    -- id is the monotonically increasing primary key.
    id INTEGER PRIMARY KEY,

    -- annual_rate is the cost-of-capital rate at time of change.
    annual_rate DOUBLE PRECISION NOT NULL,

    -- base_margin_sat is the fixed operator margin.
    base_margin_sat BIGINT NOT NULL,

    -- util_threshold_bps is the congestion threshold.
    util_threshold_bps INTEGER NOT NULL,

    -- util_spread_delta0_bps is the base congestion spread.
    util_spread_delta0_bps INTEGER NOT NULL,

    -- util_spread_delta1_bps is the linear congestion spread.
    util_spread_delta1_bps INTEGER NOT NULL,

    -- min_refresh_delta_blocks is the δ_min fee floor on refresh
    -- liquidity, expressed in blocks. Refresh liquidity fees are
    -- priced against max(δ, min_refresh_delta_blocks) to prevent
    -- a lazy-refresh bypass. See docs/fee-model.md "Fee floor δ_min".
    min_refresh_delta_blocks INTEGER NOT NULL,

    -- min_viable_policy is "reject" or "warn".
    min_viable_policy TEXT NOT NULL,

    -- min_viable_pct is the max fee-to-amount ratio.
    min_viable_pct INTEGER NOT NULL,

    -- created_at is the Unix timestamp of the change.
    created_at BIGINT NOT NULL
);
