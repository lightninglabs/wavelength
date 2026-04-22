-- Accounting tables for operator fee tracking using double-entry
-- bookkeeping.

-- Account type enum table. Each row defines a valid account
-- classification for reporting.
CREATE TABLE IF NOT EXISTS account_types (
    account_type TEXT PRIMARY KEY
);

-- Populate account types. Equity is included so operator
-- capital contributions (external funding into the treasury
-- wallet) have a balancing credit that is neither revenue nor
-- a user-owed liability.
INSERT INTO account_types (account_type) VALUES
    ('asset'),
    ('liability'),
    ('revenue'),
    ('expense'),
    ('equity')
ON CONFLICT DO NOTHING;

-- Ledger event type enum table. Each row defines a valid event
-- classification for ledger entries.
CREATE TABLE IF NOT EXISTS ledger_event_types (
    event_type TEXT PRIMARY KEY
);

-- Populate ledger event types. The set is intentionally narrow:
-- every event type here corresponds to a distinct economic event
-- that moves value between the seeded accounts. UTXO-set state
-- changes (created/spent) live in wallet_utxo_log with their own
-- classification column and do not duplicate here.
INSERT INTO ledger_event_types (event_type) VALUES
    -- Fee-model events (docs/fee-model.md).
    ('boarding_deposit'),
    ('boarding_fee'),
    ('refresh_forfeit'),
    ('refresh_new_vtxo'),
    ('refresh_fee'),
    ('offboard'),
    ('offboard_fee'),
    ('mining_fee'),
    ('round_sweep'),
    ('capital_committed'),
    -- OOR: today free, future fees will debit user claims.
    ('oor_transfer'),
    -- External operator movements into/out of the treasury wallet,
    -- driven by the wallet UTXO diff subsystem (see ledger.Actor
    -- handleBlockEpoch).
    ('external_deposit'),
    ('external_withdrawal')
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
-- Fee revenue is split per product (boarding, refresh, offboard,
-- OOR) rather than collapsed into a single operator_revenue bucket
-- so tax reporting and business analytics can see gross numbers
-- per product without parsing event_type strings. External
-- funding (operator capital contributions and withdrawals) is
-- modeled as equity.
INSERT INTO accounts (account_id, account_name, account_type) VALUES
    ('treasury_wallet',       'Treasury Wallet',        'asset'),
    ('deployed_capital',      'Deployed Capital',       'asset'),
    ('user_vtxo_claims',      'User VTXO Claims',       'liability'),
    ('boarding_fee_revenue',  'Boarding Fee Revenue',   'revenue'),
    ('refresh_fee_revenue',   'Refresh Fee Revenue',    'revenue'),
    ('offboard_fee_revenue',  'Offboard Fee Revenue',   'revenue'),
    ('oor_fee_revenue',       'OOR Fee Revenue',        'revenue'),
    ('mining_fees',           'Mining Fee Expense',     'expense'),
    ('external_funding',      'External Operator Funding', 'equity')
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

    -- round_id optionally links this entry to a specific round.
    -- Round-scoped events (boarding, refresh, offboard, mining,
    -- capital_committed, round_sweep) set this; OOR and
    -- external-wallet events do not.
    round_id BLOB,

    -- session_id optionally links this entry to a specific OOR
    -- session (32-byte identifier). OOR-scoped events set this;
    -- round-scoped events do not.
    session_id BLOB,

    -- idempotency_key is a caller-supplied opaque identifier used
    -- to make at-least-once mailbox replay a silent no-op. When
    -- set, the partial unique index below rejects a duplicate
    -- (key, event_type, debit, credit) insert; the sqlc query
    -- uses ON CONFLICT DO NOTHING so a redelivered message
    -- resolves to zero rows inserted instead of a constraint
    -- violation. Nullable because some historic callers predate
    -- idempotency and some events (external_deposit keyed on
    -- outpoint) naturally derive the key from another source.
    idempotency_key BLOB,

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
    CHECK (debit_account <> credit_account),

    -- round_id and session_id are mutually exclusive: an event
    -- belongs to at most one of them. Events without a round or
    -- session context (external deposits/withdrawals) leave both
    -- null.
    CHECK (round_id IS NULL OR session_id IS NULL)
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

CREATE INDEX IF NOT EXISTS idx_ledger_session
    ON ledger_entries(session_id);

-- Partial unique index for replay idempotency. Keyed on
-- (idempotency_key, event_type, debit, credit) so the same key
-- can legally appear for different legs of a multi-leg event
-- (e.g. a boarding deposit + fee pair sharing a round_id-derived
-- key but differing in event_type). WHERE idempotency_key IS NOT
-- NULL keeps historical and key-less callers outside the
-- uniqueness constraint.
CREATE UNIQUE INDEX IF NOT EXISTS uniq_ledger_idempotency
    ON ledger_entries(
        idempotency_key, event_type, debit_account, credit_account
    )
    WHERE idempotency_key IS NOT NULL;

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
