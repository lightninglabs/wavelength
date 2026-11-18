-- Ark channel coordination queries.

-- name: InsertArkChannel :execrows
INSERT INTO ark_channels (
    channel_id, pending_channel_id, reserved_scid, capacity,
	client_node_key, hub_node_key, client_ark_key,
	hub_ark_key, ark_operator_key, client_channel_key, hub_channel_key,
	funder_key, channel_delay, funder_delay, min_exit_delay, phase,
	oor_session_id, source_index, source_amount, source_ark_tx,
	backing_tx, channel_point_txid, channel_point_index, client_finalized,
	hub_finalized, oor_finalized, oor_aborted, backing_published,
	client_recovery_ready, hub_recovery_ready,
	source_spent_outpoint_txid,
	source_spent_outpoint_index, source_spending_txid,
	close_initiator, close_client_script, close_hub_script,
	close_fee_rate_sat_per_kw, cooperative_close_tx,
	cooperative_close_txid, close_commitment_height, close_client_balance,
	close_hub_balance, failure, revision, created_at,
	updated_at, pre_ponr_started_at
) VALUES (
	$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
	$15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27,
	$28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40,
	$41, $42, $43, $44, $45, $46,
	sqlc.arg(pre_ponr_started_at)
)
ON CONFLICT (channel_id) DO NOTHING;

-- name: GetArkChannel :one
SELECT * FROM ark_channels
WHERE channel_id = $1;

-- name: AuthorizeArkFundingBacking :execrows
UPDATE ark_channels SET authorized_backing_txid = sqlc.arg(txid),
    revision = revision + 1, updated_at = sqlc.arg(updated_at)
WHERE channel_id = sqlc.arg(channel_id) AND revision = sqlc.arg(revision)
    AND client_node_key = sqlc.arg(owner_key)
    AND authorized_backing_txid IS NULL AND phase = 2;

-- name: RecordArkFundingSignature :execrows
UPDATE ark_channels SET funding_signature = sqlc.arg(signature),
    revision = revision + 1, updated_at = sqlc.arg(updated_at)
WHERE channel_id = sqlc.arg(channel_id) AND revision = sqlc.arg(revision)
    AND client_node_key = sqlc.arg(owner_key) AND phase = 2
    AND authorized_backing_txid = sqlc.arg(txid) AND funding_signature IS NULL;

-- name: GetArkChannelByPendingID :one
SELECT * FROM ark_channels
WHERE pending_channel_id = $1;

-- name: GetArkChannelBySCID :one
SELECT * FROM ark_channels WHERE reserved_scid = $1;

-- name: GetArkChannelByChannelPoint :one
SELECT * FROM ark_channels
WHERE channel_point_txid = $1 AND channel_point_index = $2;

-- name: ListNonTerminalArkChannels :many
SELECT * FROM ark_channels
-- Phase 8 is closed and phase 10 is failed. A cooperatively closed channel
-- remains resumable because its signed replacement VTXOs still need source
-- ancestry defense. Cancelling also remains resumable.
WHERE phase NOT IN (8, 10)
    OR (phase = 8 AND cooperative_close_txid IS NOT NULL)
ORDER BY created_at ASC, channel_id ASC;

-- name: ListPeerArkChannels :many
SELECT * FROM ark_channels
WHERE client_node_key = $1 AND
    (phase NOT IN (8, 10) OR (phase = 8 AND cooperative_close_txid IS NOT NULL))
ORDER BY created_at ASC, channel_id ASC;

-- name: ListArkRecoveryPeers :many
SELECT DISTINCT client_node_key FROM ark_channels
WHERE phase NOT IN (8, 10) OR (phase = 8 AND cooperative_close_txid IS NOT NULL)
ORDER BY client_node_key;

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
	client_recovery_ready = $15,
	hub_recovery_ready = $16,
	source_spent_outpoint_txid = $17,
	source_spent_outpoint_index = $18,
	source_spending_txid = $19,
	backing_published = $20,
	close_initiator = $21,
	close_client_script = $22,
	close_hub_script = $23,
	close_fee_rate_sat_per_kw = $24,
	cooperative_close_tx = $25,
	cooperative_close_txid = $26,
	close_commitment_height = $27,
	close_client_balance = $28,
	close_hub_balance = $29,
	failure = $30,
	pre_ponr_started_at = COALESCE(
		pre_ponr_started_at, sqlc.arg(pre_ponr_started_at)
	),
	revision = revision + 1,
	updated_at = $31
WHERE channel_id = $1 AND revision = $2
    AND client_node_key = sqlc.arg(owner_key);
