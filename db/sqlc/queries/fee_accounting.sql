-- name: InsertClientLedgerEntry :exec
-- Column order matches the ledger_entries CREATE TABLE layout
-- in migration 000006 so the generated row type for SELECTs
-- stays structurally identical to the LedgerEntry model, which
-- is what the adapter returns. Changing the table column order
-- requires changing these SELECTs in lockstep.
--
-- ON CONFLICT DO NOTHING makes the insert idempotent against
-- every partial unique index on ledger_entries:
--   - idx_client_ledger_idempotent_round covers per-round events
--   - idx_client_ledger_idempotent_session covers per-OOR-session events
--   - idx_client_ledger_idempotent_key covers outpoint-keyed events
--     (unilateral exit send leg + fee leg share the same key)
-- A redelivered durable-actor message that reprocesses an entry
-- already persisted in a committed tx becomes a silent no-op
-- instead of surfacing a constraint violation that would drive
-- an infinite nack-and-retry loop on a permanent condition.
INSERT INTO ledger_entries (
    debit_account, credit_account, amount_sat,
    round_id, session_id, idempotency_key,
    event_type, description, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT DO NOTHING;

-- name: ListClientLedgerEntries :many
SELECT entry_id, debit_account, credit_account, amount_sat,
       round_id, session_id, idempotency_key,
       event_type, description, created_at
FROM ledger_entries
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: ListClientLedgerEntriesByType :many
SELECT entry_id, debit_account, credit_account, amount_sat,
       round_id, session_id, idempotency_key,
       event_type, description, created_at
FROM ledger_entries
WHERE event_type = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: ListTransactionHistory :many
-- ListTransactionHistory returns a unified newest-first history from the
-- client-side ledger and tracked boarding sweep transactions. Filters are
-- applied before LIMIT/OFFSET so filtered pagination never skips over matching
-- rows hidden behind non-matching entries.
SELECT source, entry_id, txid, transaction_type, subtype,
       amount_sat, fee_sat, created_at, status, description,
       debit_account, credit_account, round_id, session_id,
       confirmation_height
FROM (
    SELECT 0 AS source_order,
           'ledger' AS source,
           entry_id,
           NULL AS txid,
           CASE
               WHEN session_id IS NOT NULL THEN 'oor'
               WHEN event_type IN (
                   'boarding_fee_paid', 'wallet_utxo_created'
               ) THEN 'boarding'
               WHEN event_type IN (
                   'onchain_fee_paid', 'boarding_sweep_fee_paid'
               ) THEN 'sweep'
               ELSE 'round'
           END AS transaction_type,
           event_type AS subtype,
           amount_sat,
           CAST(0 AS BIGINT) AS fee_sat,
           created_at,
           CASE
               WHEN event_type = 'wallet_utxo_created' THEN 'confirmed'
               ELSE 'recorded'
           END AS status,
           description,
           debit_account,
           credit_account,
           round_id,
           session_id,
           CAST(0 AS INTEGER) AS confirmation_height
    FROM ledger_entries

    UNION ALL

    SELECT 1 AS source_order,
           'boarding_sweep' AS source,
           CAST(0 AS BIGINT) AS entry_id,
           txid,
           'sweep' AS transaction_type,
           status AS subtype,
           total_amount AS amount_sat,
           fee_amount AS fee_sat,
           created_time AS created_at,
           status,
           'boarding timeout sweep' AS description,
           '' AS debit_account,
           '' AS credit_account,
           NULL AS round_id,
           NULL AS session_id,
           COALESCE(confirmed_height, 0) AS confirmation_height
    FROM boarding_sweeps
) AS history
WHERE (sqlc.arg(type_filter) = ''
       OR transaction_type = sqlc.arg(type_filter))
  AND (sqlc.arg(from_unix_s) = 0
       OR created_at >= sqlc.arg(from_unix_s))
  AND (sqlc.arg(to_unix_s) = 0
       OR created_at <= sqlc.arg(to_unix_s))
ORDER BY created_at DESC, source_order ASC, entry_id DESC, txid DESC
LIMIT sqlc.arg(page_limit)
OFFSET sqlc.arg(page_offset);

-- name: GetClientAccountBalance :one
SELECT CAST(COALESCE(
    (SELECT SUM(le1.amount_sat) FROM ledger_entries le1
     WHERE le1.debit_account = sqlc.arg(account_id)), 0
) - COALESCE(
    (SELECT SUM(le2.amount_sat) FROM ledger_entries le2
     WHERE le2.credit_account = sqlc.arg(account_id)), 0
) AS BIGINT) AS balance;

-- name: GetTotalOperatorFeesPaid :one
-- Returns cumulative Ark protocol fees paid to the operator (fees_paid
-- account only). Does not include L1 chain/miner fees (onchain_fees).
SELECT CAST(COALESCE(SUM(amount_sat), 0) AS BIGINT) AS total_fees
FROM ledger_entries
WHERE debit_account = 'fees_paid';

-- name: CountClientLedgerEntries :one
SELECT COUNT(*) FROM ledger_entries;

-- name: ListClientAccounts :many
SELECT account_id, account_name, account_type
FROM accounts
ORDER BY account_id;
