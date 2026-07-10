-- name: UpsertPendingIntentHeader :exec
INSERT INTO pending_intents (
    intent_id,
    kind,
    requested_at_unix
) VALUES ($1, $2, $3)
ON CONFLICT (intent_id) DO UPDATE
-- Re-arm a re-persisted intent as pending. NewPendingIntentID is
-- deterministic, so retrying a terminally failed send (same inputs and
-- payload) reuses the same intent_id and lands here on the retained 'failed'
-- row. Resetting the status and failure fields makes the retry replayable
-- again; without it the replay query (which now skips non-pending rows) would
-- silently drop a retry that crashes before the round adopts it.
SET requested_at_unix = excluded.requested_at_unix,
    status = 'pending',
    failure_reason = NULL,
    failure_code = 0;

-- name: UpsertPendingBoardIntent :exec
INSERT INTO pending_board_intents (
    intent_id,
    target_vtxo_count
) VALUES ($1, $2)
ON CONFLICT (intent_id) DO UPDATE
SET target_vtxo_count = excluded.target_vtxo_count;

-- name: UpsertPendingSendIntent :exec
INSERT INTO pending_send_intents (
    intent_id,
    dest_pkscript,
    target_amount_sat,
    sweep_all,
    operator_key,
    vtxo_exit_delay,
    dust_limit_sat
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (intent_id) DO UPDATE
SET dest_pkscript = excluded.dest_pkscript,
    target_amount_sat = excluded.target_amount_sat,
    sweep_all = excluded.sweep_all,
    operator_key = excluded.operator_key,
    vtxo_exit_delay = excluded.vtxo_exit_delay,
    dust_limit_sat = excluded.dust_limit_sat;

-- name: UpsertPendingIntentAnchor :exec
INSERT INTO pending_intent_anchors (
    outpoint_hash,
    outpoint_index,
    intent_id
) VALUES ($1, $2, $3)
ON CONFLICT (outpoint_hash, outpoint_index) DO UPDATE
SET intent_id = excluded.intent_id;

-- name: ListPendingBoardIntents :many
-- Only status = 'pending' rows replay; a 'failed' intent is terminally
-- retired and must not be re-submitted on restart.
SELECT
    i.intent_id, i.requested_at_unix,
    b.target_vtxo_count
FROM pending_intents i
JOIN pending_board_intents b ON b.intent_id = i.intent_id
WHERE i.kind = 'board' AND i.status = 'pending'
ORDER BY i.requested_at_unix ASC, i.intent_id ASC;

-- name: ListPendingSendIntents :many
-- Only status = 'pending' rows replay; a 'failed' intent is terminally
-- retired and must not be re-submitted on restart.
SELECT
    i.intent_id, i.requested_at_unix,
    s.dest_pkscript, s.target_amount_sat, s.sweep_all,
    s.operator_key, s.vtxo_exit_delay, s.dust_limit_sat
FROM pending_intents i
JOIN pending_send_intents s ON s.intent_id = i.intent_id
WHERE i.kind = 'send_onchain' AND i.status = 'pending'
ORDER BY i.requested_at_unix ASC, i.intent_id ASC;

-- name: MarkPendingSendIntentFailedByOutpoint :exec
-- Terminally fail the pending send intent anchored to the given outpoint,
-- recording the reason and typed failure code. Idempotent: the status guard
-- makes a repeat call (e.g. a second forfeit outpoint of the same intent) a
-- no-op. Anchors are intentionally retained so the activity projection can
-- still correlate the failed job by its consumed outpoint.
UPDATE pending_intents
SET status = 'failed',
    failure_reason = $3,
    failure_code = $4
WHERE kind = 'send_onchain'
  AND status = 'pending'
  AND intent_id IN (
      SELECT a.intent_id FROM pending_intent_anchors a
      WHERE a.outpoint_hash = $1 AND a.outpoint_index = $2
  );

-- name: ListPendingIntentAnchorsByKind :many
SELECT a.outpoint_hash, a.outpoint_index, a.intent_id
FROM pending_intent_anchors a
JOIN pending_intents i ON i.intent_id = a.intent_id
WHERE i.kind = $1
ORDER BY a.intent_id ASC, a.outpoint_hash ASC, a.outpoint_index ASC;

-- name: ClearPendingIntentAnchorByOutpoint :exec
DELETE FROM pending_intent_anchors
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: DeleteOrphanedPendingBoardIntents :exec
DELETE FROM pending_board_intents
WHERE NOT EXISTS (
    SELECT 1 FROM pending_intent_anchors a
    WHERE a.intent_id = pending_board_intents.intent_id
);

-- name: DeleteOrphanedPendingSendIntents :exec
DELETE FROM pending_send_intents
WHERE NOT EXISTS (
    SELECT 1 FROM pending_intent_anchors a
    WHERE a.intent_id = pending_send_intents.intent_id
)
-- Keep the detail row of a terminally failed send even after its anchors are
-- gone (a released coin got reused by a later send, stealing the anchor). The
-- failed record is durable and correlated by intent_id, so its detail must
-- survive alongside its header for the activity projection to surface it.
AND NOT EXISTS (
    SELECT 1 FROM pending_intents i
    WHERE i.intent_id = pending_send_intents.intent_id
      AND i.status = 'failed'
);

-- name: DeleteOrphanedPendingIntents :exec
DELETE FROM pending_intents
WHERE NOT EXISTS (
    SELECT 1 FROM pending_intent_anchors a
    WHERE a.intent_id = pending_intents.intent_id
)
-- A terminally failed intent is a durable record, not a sweepable orphan:
-- keep its header even once its anchors are reused. Replay already skips it on
-- the status filter, and the activity projection still surfaces it as failed.
AND status <> 'failed';

-- name: DeletePendingIntentAnchorsByIntentID :exec
DELETE FROM pending_intent_anchors
WHERE intent_id = $1;

-- name: DeletePendingBoardIntentByID :exec
DELETE FROM pending_board_intents
WHERE intent_id = $1;

-- name: DeletePendingSendIntentByID :exec
DELETE FROM pending_send_intents
WHERE intent_id = $1;

-- name: DeletePendingIntentByID :exec
DELETE FROM pending_intents
WHERE intent_id = $1;

-- name: DeletePendingIntentAnchorsByKind :exec
DELETE FROM pending_intent_anchors
WHERE intent_id IN (
    SELECT intent_id FROM pending_intents WHERE kind = $1
);

-- name: DeletePendingBoardIntentsAll :exec
DELETE FROM pending_board_intents;

-- name: DeletePendingSendIntentsAll :exec
DELETE FROM pending_send_intents;

-- name: DeletePendingIntentsByKind :exec
DELETE FROM pending_intents
WHERE kind = $1;
