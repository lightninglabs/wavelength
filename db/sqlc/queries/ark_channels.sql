-- Ark channel coordination queries.

-- name: InsertArkChannel :execrows
INSERT INTO ark_channels (
    channel_id, kind, funder, pending_channel_id, reserved_scid, capacity,
	client_node_key, hub_node_key, payment_hash, client_ark_key,
	hub_ark_key, ark_operator_key, client_channel_key, hub_channel_key,
	funder_key, channel_delay, funder_delay, min_exit_delay, phase,
	oor_session_id, source_index, source_amount, source_ark_tx,
	backing_tx, channel_point_txid, channel_point_index, client_finalized,
	hub_finalized, oor_finalized, oor_aborted, backing_published,
	close_initiator, close_client_script, close_hub_script,
	close_fee_rate_sat_per_kw, cooperative_close_tx,
	cooperative_close_txid, close_commitment_height, close_client_balance,
	close_hub_balance, client_close_signed, hub_close_signed,
	client_close_finalized, hub_close_finalized, failure, revision, created_at,
	updated_at
) VALUES (
	$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
	$15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27,
	$28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40,
	$41, $42, $43, $44, $45, $46, $47, $48
)
ON CONFLICT (channel_id) DO NOTHING;

-- name: GetArkChannel :one
SELECT * FROM ark_channels
WHERE channel_id = $1;

-- name: GetArkChannelByPendingID :one
SELECT * FROM ark_channels
WHERE pending_channel_id = $1;

-- name: GetArkChannelByChannelPoint :one
SELECT * FROM ark_channels
WHERE channel_point_txid = $1 AND channel_point_index = $2;

-- name: ListNonTerminalArkChannels :many
SELECT * FROM ark_channels
-- Phase 8 is closed and phase 10 is failed. Cancelling remains resumable.
WHERE phase NOT IN (8, 10)
ORDER BY created_at ASC, channel_id ASC;

-- name: CompareAndSwapArkChannel :execrows
UPDATE ark_channels SET
    phase = $3,
	oor_session_id = $4,
	source_index = $5,
	source_amount = $6,
	source_ark_tx = $7,
	backing_tx = $8,
	channel_point_txid = $9,
	channel_point_index = $10,
	client_finalized = $11,
	hub_finalized = $12,
	oor_finalized = $13,
	oor_aborted = $14,
	backing_published = $15,
	close_initiator = $16,
	close_client_script = $17,
	close_hub_script = $18,
	close_fee_rate_sat_per_kw = $19,
	cooperative_close_tx = $20,
	cooperative_close_txid = $21,
	close_commitment_height = $22,
	close_client_balance = $23,
	close_hub_balance = $24,
	client_close_signed = $25,
	hub_close_signed = $26,
	client_close_finalized = $27,
	hub_close_finalized = $28,
	failure = $29,
	revision = revision + 1,
	updated_at = $30
WHERE channel_id = $1 AND revision = $2;
