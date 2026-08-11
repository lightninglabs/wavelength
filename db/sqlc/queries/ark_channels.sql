-- Ark channel coordination queries.

-- name: InsertArkChannel :execrows
INSERT INTO ark_channels (
    channel_id, kind, funder, pending_channel_id, reserved_scid, capacity,
    client_node_key, hub_node_key, payment_hash, client_ark_key,
    hub_ark_key, ark_operator_key, client_channel_key, hub_channel_key,
    funder_key, channel_delay, funder_delay, min_exit_delay, phase,
    source_txid, source_index, source_amount, round_id, commitment_txid,
    backing_tx, channel_point_txid, channel_point_index, client_finalized,
    hub_finalized, round_committed, round_confirmed, backing_published,
    failure, revision, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
    $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27,
    $28, $29, $30, $31, $32, $33, $34, $35, $36
)
ON CONFLICT (channel_id) DO NOTHING;

-- name: GetArkChannel :one
SELECT * FROM ark_channels
WHERE channel_id = $1;

-- name: GetArkChannelByPendingID :one
SELECT * FROM ark_channels
WHERE pending_channel_id = $1;

-- name: ListNonTerminalArkChannels :many
SELECT * FROM ark_channels
-- Phase 9 is closed and phase 11 is failed. Cancelling remains resumable.
WHERE phase NOT IN (9, 11)
ORDER BY created_at ASC, channel_id ASC;

-- name: CompareAndSwapArkChannel :execrows
UPDATE ark_channels SET
    phase = $3,
    source_txid = $4,
    source_index = $5,
    source_amount = $6,
    round_id = $7,
    commitment_txid = $8,
    backing_tx = $9,
    channel_point_txid = $10,
    channel_point_index = $11,
    client_finalized = $12,
    hub_finalized = $13,
    round_committed = $14,
    round_confirmed = $15,
    backing_published = $16,
    failure = $17,
    revision = revision + 1,
    updated_at = $18
WHERE channel_id = $1 AND revision = $2;
