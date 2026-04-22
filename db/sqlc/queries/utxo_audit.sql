-- name: InsertWalletUTXOLog :execrows
-- The UNIQUE(outpoint_hash, outpoint_index, event) constraint
-- plus ON CONFLICT DO NOTHING makes the per-block UTXO diff
-- loop crash-safe: a redelivered mailbox message or a
-- recomputed diff over the same block rewrites the same rows
-- without raising a constraint violation. :execrows returns
-- the rowcount so the diff loop can tell whether a write
-- landed (new UTXO change) or was silently deduped (replay).
--
-- source_id is NULL for rows the diff loop produced itself and
-- is the 16-byte round_id / batch_id for pre-inserts from the
-- round / sweep handlers.
INSERT INTO wallet_utxo_log (
    outpoint_hash, outpoint_index, amount_sat,
    event, block_height, classified_as, created_at, source_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT DO NOTHING;

-- name: ListWalletUTXOLog :many
SELECT entry_id, outpoint_hash, outpoint_index, amount_sat,
       event, block_height, classified_as, created_at,
       source_id
FROM wallet_utxo_log
ORDER BY created_at DESC, entry_id DESC
LIMIT $1 OFFSET $2;

-- name: ListWalletUTXOLogByBlock :many
-- TODO(fees-03): add LIMIT/OFFSET when Admin RPC pagination lands.
SELECT entry_id, outpoint_hash, outpoint_index, amount_sat,
       event, block_height, classified_as, created_at,
       source_id
FROM wallet_utxo_log
WHERE block_height = $1
ORDER BY entry_id;

-- name: CountWalletUTXOLog :one
SELECT COUNT(*) FROM wallet_utxo_log;

-- name: ListWalletUTXOLogByClassification :many
SELECT entry_id, outpoint_hash, outpoint_index, amount_sat,
       event, block_height, classified_as, created_at,
       source_id
FROM wallet_utxo_log
WHERE classified_as = $1
ORDER BY created_at DESC, entry_id DESC
LIMIT $2 OFFSET $3;

-- name: GetWalletUTXOLogByOutpointEvent :one
-- Returns the single audit row for a given (outpoint, event)
-- triple if present, or sql.ErrNoRows otherwise.
--
-- NOTE: unused by the classifier hot path today -- the diff
-- loop relies on the ON CONFLICT DO NOTHING rowcount from
-- InsertWalletUTXOLog to detect whether a round / sweep
-- handler pre-inserted the attribution row, avoiding a second
-- round-trip per outpoint. This query is kept for offline
-- reconciliation tooling (audit scripts, ops inspection) and
-- for tests that want to assert individual row shape without
-- walking the whole live-utxo reconstruction. If a future
-- caller is added, update this comment so the reserved
-- intent is obvious.
SELECT entry_id, outpoint_hash, outpoint_index, amount_sat,
       event, block_height, classified_as, created_at,
       source_id
FROM wallet_utxo_log
WHERE outpoint_hash = $1
  AND outpoint_index = $2
  AND event = $3;

-- name: PromotePendingWalletUTXOLog :many
-- Promote every audit row in the 'pending' limbo state whose
-- block_height is strictly below the given watermark into its
-- terminal classification. The diff loop inserts 'pending' rows
-- for outpoints it observes on a block epoch that has not yet
-- seen its matching RoundConfirmedMsg / SweepCompletedMsg; the
-- reconciliation pass at the NEXT block epoch flips still-
-- pending rows to 'deposit' (created) or 'withdrawal' (spent)
-- and returns them so the classifier can book the matching
-- external_* ledger leg in the same transaction.
UPDATE wallet_utxo_log
SET classified_as = CASE
        WHEN event = 'created' THEN 'deposit'
        WHEN event = 'spent' THEN 'withdrawal'
        ELSE classified_as
    END
WHERE classified_as = 'pending'
  AND block_height < $1
RETURNING entry_id, outpoint_hash, outpoint_index,
          amount_sat, event, block_height, classified_as,
          created_at, source_id;

-- name: ListLiveWalletUTXOs :many
-- Reconstruct the current wallet UTXO set from the audit log:
-- every (outpoint_hash, outpoint_index) that has a 'created'
-- row without a corresponding 'spent' row is considered live.
-- The ledger actor's per-block diff subsystem calls this on
-- startup to rehydrate its in-memory snapshot so a restart does
-- not silently re-enter the seeding pass and swallow external
-- deposits that arrived during downtime.
--
-- The schema's UNIQUE(hash, index, event) constraint means at
-- most one 'created' and one 'spent' row exist per outpoint,
-- which keeps this query O(n) over the log rather than
-- quadratic.
SELECT c.outpoint_hash, c.outpoint_index, c.amount_sat,
       c.block_height
FROM wallet_utxo_log c
WHERE c.event = 'created'
  AND NOT EXISTS (
      SELECT 1 FROM wallet_utxo_log s
      WHERE s.outpoint_hash = c.outpoint_hash
        AND s.outpoint_index = c.outpoint_index
        AND s.event = 'spent'
  )
ORDER BY c.entry_id;
