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
       confirmation_height, output_index, boarding_address
FROM (
    SELECT 0 AS source_order,
           'ledger' AS source,
           le.entry_id,
           COALESCE(le.chain_txid, oor_created.outpoint_hash) AS txid,
           CASE
               WHEN le.session_id IS NOT NULL THEN 'oor'
               WHEN le.event_type = 'vtxo_received'
                    AND le.round_id IS NULL
                    AND oor_created.session_id IS NOT NULL
                    AND le.debit_account = 'vtxo_balance'
                    AND le.credit_account = 'transfers_in'
               THEN 'oor'
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
               WHEN le.event_type = 'wallet_utxo_created'
                    AND boarding_round.confirmed
               THEN 'confirmed'
               WHEN le.event_type = 'wallet_utxo_created'
                    AND (le.chain_txid IS NULL OR le.chain_vout IS NULL)
               THEN 'confirmed'
               WHEN le.event_type = 'wallet_utxo_created'
                    AND NOT EXISTS (
                        SELECT 1
                        FROM boarding_intents AS status_bi
                        WHERE status_bi.outpoint_hash = le.chain_txid
                          AND status_bi.outpoint_index = le.chain_vout
                    )
               THEN 'confirmed'
               WHEN le.event_type = 'wallet_utxo_created' THEN 'boarding'
               ELSE 'recorded'
           END AS status,
           le.description,
           le.debit_account,
           le.credit_account,
           le.round_id,
           COALESCE(le.session_id, oor_created.session_id) AS session_id,
           COALESCE(le.confirmation_height, 0) AS confirmation_height,
           COALESCE(le.chain_vout, oor_created.outpoint_index, -1) AS
               output_index,
           -- boarding_address links a confirmed boarding-deposit
           -- (wallet_utxo_created) row back to the allocated boarding
           -- address, so the client can key the confirmed DEPOSIT row by
           -- the same stable id as its pending row. Empty ('') for every
           -- other event type; the CASE/COALESCE never yields NULL.
           CASE
               WHEN le.event_type = 'wallet_utxo_created'
               THEN COALESCE(deposit_ba.address_string, '')
               ELSE ''
           END AS boarding_address
    FROM ledger_entries AS le
    LEFT JOIN oor_vtxo_bindings AS oor_created
        ON oor_created.outpoint_hash = le.chain_txid
       AND oor_created.outpoint_index = le.chain_vout
       AND oor_created.link_kind = 0
    LEFT JOIN (
        SELECT allocated.outpoint_hash,
               allocated.outpoint_index,
               allocated.confirmed,
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
                   proportional.confirmed,
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
                       r.status = 'confirmed' AS confirmed,
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
                JOIN rounds AS r
                    ON r.round_id = rbi.round_id
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
    LEFT JOIN boarding_intents AS deposit_bi
        ON deposit_bi.outpoint_hash = le.chain_txid
       AND deposit_bi.outpoint_index = le.chain_vout
    LEFT JOIN boarding_addresses AS deposit_ba
        ON deposit_ba.pk_script = deposit_bi.pk_script

    UNION ALL

    SELECT 0 AS source_order,
           'oor_binding' AS source,
           -CAST(ROW_NUMBER() OVER (
               ORDER BY b.session_id, b.output_index,
                        b.outpoint_hash, b.outpoint_index
           ) AS BIGINT) AS entry_id,
           b.outpoint_hash AS txid,
           'oor' AS transaction_type,
           'vtxo_received' AS subtype,
           v.amount AS amount_sat,
           CAST(0 AS BIGINT) AS fee_sat,
           b.created_at,
           'recorded' AS status,
           'OOR VTXO created' AS description,
           'vtxo_balance' AS debit_account,
           'transfers_in' AS credit_account,
           NULL AS round_id,
           b.session_id,
           CAST(0 AS INTEGER) AS confirmation_height,
           b.outpoint_index AS output_index,
           '' AS boarding_address
    FROM oor_vtxo_bindings AS b
    JOIN vtxos AS v
        ON v.outpoint_hash = b.outpoint_hash
       AND v.outpoint_index = b.outpoint_index
    WHERE b.link_kind = 0
      AND NOT EXISTS (
          SELECT 1
          FROM ledger_entries AS le_existing
          WHERE le_existing.event_type = 'vtxo_received'
            AND le_existing.chain_txid = b.outpoint_hash
            AND le_existing.chain_vout = b.outpoint_index
            AND le_existing.debit_account = 'vtxo_balance'
            AND le_existing.credit_account = 'transfers_in'
      )

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
           CAST(-1 AS INTEGER) AS output_index,
           '' AS boarding_address
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

-- name: ListClientAccountBalances :many
SELECT a.account_id,
       a.account_name,
       a.account_type,
       CAST(COALESCE(SUM(
           CASE
               WHEN le.debit_account = a.account_id THEN le.amount_sat
               WHEN le.credit_account = a.account_id THEN -le.amount_sat
               ELSE CAST(0 AS BIGINT)
           END
       ), 0) AS BIGINT) AS balance_sat
FROM accounts AS a
LEFT JOIN ledger_entries AS le
    ON le.debit_account = a.account_id
    OR le.credit_account = a.account_id
GROUP BY a.account_id, a.account_name, a.account_type
ORDER BY a.account_id;

-- name: ListClientLedgerEventTotals :many
SELECT event_type,
       CAST(COUNT(*) AS BIGINT) AS entry_count,
       CAST(COALESCE(SUM(amount_sat), 0) AS BIGINT) AS total_sat
FROM ledger_entries
GROUP BY event_type
ORDER BY event_type;

-- name: GetClientLedgerStats :one
SELECT CAST(COUNT(*) AS BIGINT) AS entry_count,
       CAST(COALESCE(MIN(created_at), 0) AS BIGINT) AS first_created_at,
       CAST(COALESCE(MAX(created_at), 0) AS BIGINT) AS last_created_at
FROM ledger_entries;
