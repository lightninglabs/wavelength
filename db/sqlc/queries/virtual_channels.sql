-- name: InsertVirtualChannel :exec
INSERT INTO virtual_channels (
	virtual_channel_id, pending_channel_id,
	channel_point_hash, channel_point_index, remote_node_pubkey,
	role, status, capacity_sat, local_balance_sat, remote_balance_sat,
	backing_tx, funding_psbt, created_at, updated_at
) VALUES (
	$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14
);

-- name: InsertVirtualChannelVTXO :exec
INSERT INTO virtual_channel_vtxos (
	virtual_channel_id, outpoint_hash, outpoint_index, amount_sat
) VALUES (
	$1, $2, $3, $4
);

-- name: InsertVirtualChannelIntent :exec
INSERT INTO virtual_channel_intents (
	pending_channel_id, remote_node_pubkey, role, status, capacity_sat,
	local_balance_sat, remote_balance_sat, created_at, updated_at
) VALUES (
	$1, $2, $3, $4, $5, $6, $7, $8, $9
);

-- name: InsertVirtualChannelIntentVTXO :exec
INSERT INTO virtual_channel_intent_vtxos (
	pending_channel_id, outpoint_hash, outpoint_index, amount_sat
) VALUES (
	$1, $2, $3, $4
);

-- name: GetVirtualChannel :one
SELECT * FROM virtual_channels
WHERE virtual_channel_id = $1;

-- name: GetVirtualChannelByPendingID :one
SELECT * FROM virtual_channels
WHERE pending_channel_id = $1;

-- name: GetVirtualChannelIntentByPendingID :one
SELECT * FROM virtual_channel_intents
WHERE pending_channel_id = $1;

-- name: GetVirtualChannelByChannelPoint :one
SELECT * FROM virtual_channels
WHERE channel_point_hash = $1 AND channel_point_index = $2;

-- name: ListVirtualChannelsByChannelPointHash :many
SELECT * FROM virtual_channels
WHERE channel_point_hash = $1
ORDER BY channel_point_index;

-- name: ListVirtualChannelsByStatus :many
SELECT * FROM virtual_channels
WHERE status = $1
ORDER BY updated_at DESC;

-- name: ListVirtualChannelVTXOs :many
SELECT * FROM virtual_channel_vtxos
WHERE virtual_channel_id = $1
ORDER BY outpoint_hash, outpoint_index;

-- name: ListVirtualChannelIntentVTXOs :many
SELECT * FROM virtual_channel_intent_vtxos
WHERE pending_channel_id = $1
ORDER BY outpoint_hash, outpoint_index;

-- name: DeleteVirtualChannelIntent :execrows
DELETE FROM virtual_channel_intents
WHERE pending_channel_id = $1;

-- name: UpdateVirtualChannelStatus :execrows
UPDATE virtual_channels
SET status = $2, updated_at = $3
WHERE virtual_channel_id = $1
	AND status != $2;

-- name: MarkVirtualChannelActive :execrows
UPDATE virtual_channels
SET status = 'active', backing_tx = $2, updated_at = $3
WHERE virtual_channel_id = $1
	AND channel_point_hash = $4
	AND status = 'negotiating';

-- name: MarkVirtualChannelFailed :execrows
UPDATE virtual_channels
SET status = 'failed', updated_at = $2
WHERE virtual_channel_id = $1
	AND status = 'negotiating';

-- name: MarkVirtualChannelMaterializing :execrows
UPDATE virtual_channels
SET status = 'materializing', updated_at = $2, materialized_at = $3
WHERE virtual_channel_id = $1
	AND status IN ('active', 'closing');

-- name: MarkVirtualChannelClosing :execrows
UPDATE virtual_channels
SET status = 'closing', updated_at = $2
WHERE virtual_channel_id = $1
	AND status IN ('active', 'materializing');

-- name: MarkVirtualChannelClosed :execrows
UPDATE virtual_channels
SET status = 'closed', updated_at = $2, closed_at = $3
WHERE virtual_channel_id = $1
	AND status IN ('active', 'materializing', 'closing');

-- name: MarkVirtualChannelCoopClosed :execrows
UPDATE virtual_channels
SET status = 'closed',
	local_balance_sat = $2,
	remote_balance_sat = $3,
	close_tx = $4,
	updated_at = $5,
	closed_at = $6
WHERE virtual_channel_id = $1
	AND status IN ('active', 'closing')
	AND $2 + $3 <= capacity_sat;
