-- name: UpsertPendingIntentHeader :exec
INSERT INTO pending_intents (
    intent_id,
    kind,
    requested_at_unix
) VALUES ($1, $2, $3)
ON CONFLICT (intent_id) DO UPDATE
SET requested_at_unix = excluded.requested_at_unix;

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
SELECT
    i.intent_id, i.requested_at_unix,
    b.target_vtxo_count
FROM pending_intents i
JOIN pending_board_intents b ON b.intent_id = i.intent_id
WHERE i.kind = 'board'
ORDER BY i.requested_at_unix ASC, i.intent_id ASC;

-- name: ListPendingSendIntents :many
SELECT
    i.intent_id, i.requested_at_unix,
    s.dest_pkscript, s.target_amount_sat, s.sweep_all,
    s.operator_key, s.vtxo_exit_delay, s.dust_limit_sat
FROM pending_intents i
JOIN pending_send_intents s ON s.intent_id = i.intent_id
WHERE i.kind = 'send_onchain'
ORDER BY i.requested_at_unix ASC, i.intent_id ASC;

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
);

-- name: DeleteOrphanedPendingIntents :exec
DELETE FROM pending_intents
WHERE NOT EXISTS (
    SELECT 1 FROM pending_intent_anchors a
    WHERE a.intent_id = pending_intents.intent_id
);

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
