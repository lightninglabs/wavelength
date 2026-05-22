-- name: InsertClientLedgerEntry :exec
-- Column order matches the ledger_entries CREATE TABLE layout
-- from migration 000006 plus the chain metadata columns added in
-- migration 000014, so the generated row type for SELECTs stays
-- structurally identical to the LedgerEntry model returned by the
-- adapter. Changing the table column order requires changing these
-- SELECTs in lockstep.
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
    event_type, description, created_at,
    chain_txid, chain_vout, confirmation_height
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT DO NOTHING;

-- name: ListClientLedgerEntries :many
SELECT entry_id, debit_account, credit_account, amount_sat,
       round_id, session_id, idempotency_key,
       event_type, description, created_at,
       chain_txid, chain_vout, confirmation_height
FROM ledger_entries
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: ListClientLedgerEntriesByType :many
SELECT entry_id, debit_account, credit_account, amount_sat,
       round_id, session_id, idempotency_key,
       event_type, description, created_at,
       chain_txid, chain_vout, confirmation_height
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
       confirmation_height, output_index
FROM (
    SELECT 0 AS source_order,
           'ledger' AS source,
           le.entry_id,
           le.chain_txid AS txid,
           CASE
               WHEN le.session_id IS NOT NULL THEN 'oor'
               WHEN le.event_type IN (
                   'boarding_fee_paid', 'wallet_utxo_created'
               ) THEN 'boarding'
               WHEN le.event_type IN (
                   'onchain_fee_paid', 'boarding_sweep_fee_paid'
               ) THEN 'sweep'
               ELSE 'round'
           END AS transaction_type,
           le.event_type AS subtype,
           le.amount_sat,
           CASE
               WHEN le.event_type = 'wallet_utxo_created'
                    AND boarding_round.fee_sat > 0
               THEN boarding_round.fee_sat
               ELSE CAST(0 AS BIGINT)
           END AS fee_sat,
           le.created_at,
           CASE
               WHEN le.event_type = 'wallet_utxo_created' THEN 'confirmed'
               ELSE 'recorded'
           END AS status,
           le.description,
           le.debit_account,
           le.credit_account,
           le.round_id,
           le.session_id,
           COALESCE(le.confirmation_height, 0) AS confirmation_height,
           COALESCE(le.chain_vout, -1) AS output_index
    FROM ledger_entries AS le
    LEFT JOIN (
        SELECT allocated.outpoint_hash,
               allocated.outpoint_index,
               allocated.base_fee_sat +
                   CASE
                       WHEN allocated.remainder_rank <=
                            allocated.leftover_fee_sat
                       THEN 1
                       ELSE 0
                   END AS fee_sat
        FROM (
            SELECT proportional.outpoint_hash,
                   proportional.outpoint_index,
                   proportional.base_fee_sat,
                   proportional.round_fee_sat -
                       SUM(proportional.base_fee_sat) OVER (
                           PARTITION BY proportional.round_id
                       ) AS leftover_fee_sat,
                   ROW_NUMBER() OVER (
                       PARTITION BY proportional.round_id
                       ORDER BY proportional.remainder_sat DESC,
                                proportional.outpoint_hash,
                                proportional.outpoint_index
                   ) AS remainder_rank
            FROM (
                SELECT rbi.round_id,
                       rbi.outpoint_hash,
                       rbi.outpoint_index,
                       CASE
                           WHEN stats.round_fee_sat > 0
                                AND stats.input_amount_sat > 0
                           THEN (stats.round_fee_sat * bi.amount) /
                                stats.input_amount_sat
                           ELSE CAST(0 AS BIGINT)
                       END AS base_fee_sat,
                       CASE
                           WHEN stats.round_fee_sat > 0
                                AND stats.input_amount_sat > 0
                           THEN (stats.round_fee_sat * bi.amount) %
                                stats.input_amount_sat
                           ELSE CAST(0 AS BIGINT)
                       END AS remainder_sat,
                       stats.round_fee_sat
                FROM round_boarding_intents AS rbi
                JOIN boarding_intents AS bi
                    USING (outpoint_hash, outpoint_index)
                JOIN (
                    SELECT intent_sums.round_id,
                           CAST(CASE
                               WHEN vtxo_sums.vtxo_amount_sat IS NOT NULL
                                    AND vtxo_sums.vtxo_amount_sat > 0
                                    AND vtxo_sums.vtxo_amount_sat <
                                        intent_sums.input_amount_sat
                               THEN intent_sums.input_amount_sat -
                                    vtxo_sums.vtxo_amount_sat
                               ELSE CAST(0 AS BIGINT)
                           END AS BIGINT) AS round_fee_sat,
                           intent_sums.input_amount_sat
                    FROM (
                        SELECT round_id,
                               CAST(SUM(amount) AS BIGINT) AS
                                   input_amount_sat
                        FROM round_boarding_intents
                        JOIN boarding_intents
                            USING (outpoint_hash, outpoint_index)
                        GROUP BY round_id
                    ) AS intent_sums
                    LEFT JOIN (
                        SELECT round_id,
                               CAST(SUM(amount) AS BIGINT) AS vtxo_amount_sat
                        FROM vtxos
                        GROUP BY round_id
                    ) AS vtxo_sums ON vtxo_sums.round_id =
                        intent_sums.round_id
                ) AS stats ON stats.round_id = rbi.round_id
            ) AS proportional
        ) AS allocated
    ) AS boarding_round
        ON boarding_round.outpoint_hash = le.chain_txid
       AND boarding_round.outpoint_index = le.chain_vout

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
           COALESCE(confirmed_height, 0) AS confirmation_height,
           CAST(-1 AS INTEGER) AS output_index
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
