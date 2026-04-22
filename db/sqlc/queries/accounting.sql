-- name: InsertLedgerEntry :execrows
-- ON CONFLICT DO NOTHING makes at-least-once mailbox replay a
-- silent no-op: a duplicate (idempotency_key, event_type,
-- debit_account, credit_account) inserts zero rows instead of
-- raising a uniqueness violation that would drive the durable
-- actor into an endless retry loop. Entries without an
-- idempotency_key are outside the partial unique index and
-- always insert. The :execrows mode returns rowcount so callers
-- can log (or surface) whether the insert was accepted or
-- silently deduped.
INSERT INTO ledger_entries (
    debit_account, credit_account, amount_sat,
    round_id, session_id, idempotency_key,
    event_type, description, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT DO NOTHING;

-- name: ListLedgerEntries :many
SELECT entry_id, debit_account, credit_account, amount_sat,
       round_id, session_id, idempotency_key,
       event_type, description, created_at
FROM ledger_entries
ORDER BY created_at DESC, entry_id DESC
LIMIT $1 OFFSET $2;

-- name: ListLedgerEntriesByRound :many
-- TODO(fees-03): add LIMIT/OFFSET when Admin RPC pagination lands.
SELECT entry_id, debit_account, credit_account, amount_sat,
       round_id, session_id, idempotency_key,
       event_type, description, created_at
FROM ledger_entries
WHERE round_id = $1
ORDER BY created_at DESC;

-- name: ListLedgerEntriesBySession :many
SELECT entry_id, debit_account, credit_account, amount_sat,
       round_id, session_id, idempotency_key,
       event_type, description, created_at
FROM ledger_entries
WHERE session_id = $1
ORDER BY created_at DESC;

-- name: ListLedgerEntriesByEventType :many
SELECT entry_id, debit_account, credit_account, amount_sat,
       round_id, session_id, idempotency_key,
       event_type, description, created_at
FROM ledger_entries
WHERE event_type = $1
ORDER BY created_at DESC, entry_id DESC
LIMIT $2 OFFSET $3;

-- name: GetAccountBalance :one
-- Single-pass conditional aggregation: scans the table once and sums
-- +amount_sat for debits and -amount_sat for credits of the target
-- account. The explicit CAST forces sqlc to infer int64, which is
-- required because BIGINT sums can exceed int32's ~21.47 BTC limit.
SELECT CAST(COALESCE(SUM(
    CASE
        WHEN debit_account = sqlc.arg('account_id') THEN amount_sat
        WHEN credit_account = sqlc.arg('account_id') THEN -amount_sat
        ELSE 0
    END
), 0) AS BIGINT) AS balance
FROM ledger_entries
WHERE debit_account = sqlc.arg('account_id')
   OR credit_account = sqlc.arg('account_id');

-- name: CountLedgerEntries :one
SELECT COUNT(*) FROM ledger_entries;

-- name: ListAccounts :many
SELECT account_id, account_name, account_type
FROM accounts
ORDER BY account_id;

-- name: InsertFeeScheduleHistory :exec
INSERT INTO fee_schedule_history (
    annual_rate, base_margin_sat, util_threshold_bps,
    util_spread_delta0_bps, util_spread_delta1_bps,
    min_refresh_delta_blocks, min_viable_policy, min_viable_pct,
    created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: ListFeeScheduleHistory :many
SELECT id, annual_rate, base_margin_sat, util_threshold_bps,
       util_spread_delta0_bps, util_spread_delta1_bps,
       min_refresh_delta_blocks, min_viable_policy, min_viable_pct,
       created_at
FROM fee_schedule_history
ORDER BY created_at DESC
LIMIT $1;
