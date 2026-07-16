-- name: InsertVirtualChannel :exec
INSERT INTO virtual_channels (
	virtual_channel_id, pending_channel_id,
	channel_point_hash, channel_point_index, remote_node_pubkey,
	role, status, capacity_sat, local_balance_sat, remote_balance_sat,
	backing_tx, funding_psbt, created_at, updated_at,
	kind, round_id, state_version
) VALUES (
	$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
	$15, $16, $17
);

-- name: InsertVirtualChannelVTXO :exec
INSERT INTO virtual_channel_vtxos (
	virtual_channel_id, outpoint_hash, outpoint_index, amount_sat,
	pk_script, policy_template
) VALUES (
	$1, $2, $3, $4, $5, $6
);

-- name: InsertVirtualChannelIntent :exec
INSERT INTO virtual_channel_intents (
	pending_channel_id, remote_node_pubkey, role, status, capacity_sat,
	local_balance_sat, remote_balance_sat, created_at, updated_at,
	kind, round_id, request_key, state_version
) VALUES (
	$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
);

-- name: InsertVirtualChannelIntentVTXO :exec
INSERT INTO virtual_channel_intent_vtxos (
	pending_channel_id, outpoint_hash, outpoint_index, amount_sat,
	pk_script, policy_template
) VALUES (
	$1, $2, $3, $4, $5, $6
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

-- name: ListVirtualChannelIntentsByStatus :many
SELECT * FROM virtual_channel_intents
WHERE status = $1
ORDER BY updated_at;

-- name: GetVirtualChannelByChannelPoint :one
SELECT * FROM virtual_channels
WHERE channel_point_hash = $1 AND channel_point_index = $2;

-- name: GetVirtualChannelByBackingVTXO :one
SELECT virtual_channels.*
FROM virtual_channels
JOIN virtual_channel_vtxos
	ON virtual_channel_vtxos.virtual_channel_id =
		virtual_channels.virtual_channel_id
WHERE virtual_channel_vtxos.outpoint_hash = $1
	AND virtual_channel_vtxos.outpoint_index = $2
LIMIT 1;

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

-- name: CountVirtualChannelBackingOwners :one
SELECT (
	SELECT COUNT(*) FROM virtual_channel_vtxos AS channels
	WHERE channels.outpoint_hash = $1 AND channels.outpoint_index = $2
) + (
	SELECT COUNT(*) FROM virtual_channel_intent_vtxos AS intents
	WHERE intents.outpoint_hash = $1 AND intents.outpoint_index = $2
) AS owner_count;

-- name: DeleteVirtualChannelIntentCAS :execrows
DELETE FROM virtual_channel_intents
WHERE pending_channel_id = $1
	AND status = $2
	AND state_version = $3;

-- name: DeleteVirtualChannelIntentVTXOs :exec
DELETE FROM virtual_channel_intent_vtxos
WHERE pending_channel_id = $1;

-- name: DeleteVirtualChannelVTXOs :exec
DELETE FROM virtual_channel_vtxos
WHERE virtual_channel_id = $1;

-- name: TransitionVirtualChannelIntent :execrows
UPDATE virtual_channel_intents
SET status = $4,
	state_version = state_version + 1,
	updated_at = $5
WHERE pending_channel_id = $1
	AND status = $2
	AND state_version = $3;

-- name: BindVirtualChannelIntent :execrows
UPDATE virtual_channel_intents
SET status = 'funding_bound',
	round_id = $4,
	state_version = state_version + 1,
	updated_at = $5
WHERE pending_channel_id = $1
	AND status = 'round_requested'
	AND state_version = $2
	AND round_id IS NULL
	AND kind = $3;

-- name: TransitionVirtualChannel :execrows
UPDATE virtual_channels
SET status = $4,
	state_version = state_version + 1,
	updated_at = $5
WHERE virtual_channel_id = $1
	AND status = $2
	AND state_version = $3;

-- name: ArmVirtualChannelBacking :execrows
UPDATE virtual_channels
SET status = 'backing_armed',
	backing_tx = $4,
	state_version = state_version + 1,
	updated_at = $5,
	backing_armed_at = $5
WHERE virtual_channel_id = $1
	AND status = 'funding_verified'
	AND state_version = $2
	AND channel_point_hash = $3;

-- name: ConfirmRoundVirtualChannels :execrows
UPDATE virtual_channels
SET status = 'round_confirmed',
	state_version = state_version + 1,
	updated_at = $2
WHERE kind = 'receive_channel'
	AND round_id = $1
	AND status = 'backing_armed';

-- name: ActivateConfirmedRoundVirtualChannels :execrows
UPDATE virtual_channels
SET status = 'active',
	state_version = state_version + 1,
	updated_at = $2
WHERE kind = 'receive_channel'
	AND round_id = $1
	AND status = 'round_confirmed';

-- name: ActivateAllConfirmedVirtualChannels :execrows
UPDATE virtual_channels
SET status = 'active',
	state_version = state_version + 1,
	updated_at = $1
WHERE kind = 'receive_channel'
	AND status = 'round_confirmed';

-- name: FailRoundVirtualChannels :execrows
UPDATE virtual_channels
SET status = 'failed',
	state_version = state_version + 1,
	updated_at = $2
WHERE kind = 'receive_channel'
	AND round_id = $1
	AND status IN (
		'funding_bound', 'lnd_negotiating', 'funding_verified'
	);

-- name: CloseFailedRoundArmedVirtualChannels :execrows
UPDATE virtual_channels
SET status = 'closing',
	state_version = state_version + 1,
	updated_at = $2
WHERE kind = 'receive_channel'
	AND round_id = $1
	AND status = 'backing_armed';

-- name: FailRoundVirtualChannelIntents :execrows
UPDATE virtual_channel_intents
SET status = 'failed',
	state_version = state_version + 1,
	updated_at = $2
WHERE kind = 'receive_channel'
	AND round_id = $1
	AND status IN ('funding_bound', 'lnd_negotiating');

-- name: ReleaseFailedRoundVirtualChannelVTXOs :exec
DELETE FROM virtual_channel_vtxos
WHERE virtual_channel_id IN (
	SELECT virtual_channel_id
	FROM virtual_channels
	WHERE kind = 'receive_channel'
		AND round_id = $1
		AND status = 'failed'
		AND backing_armed_at IS NULL
);

-- name: ReleaseFailedRoundVirtualChannelIntentVTXOs :exec
DELETE FROM virtual_channel_intent_vtxos
WHERE pending_channel_id IN (
	SELECT pending_channel_id
	FROM virtual_channel_intents
	WHERE kind = 'receive_channel'
		AND round_id = $1
		AND status = 'failed'
);

-- name: MarkVirtualChannelMaterializing :execrows
UPDATE virtual_channels
SET status = 'materializing',
	state_version = state_version + 1,
	updated_at = $4,
	materialized_at = $5
WHERE virtual_channel_id = $1
	AND status = $2
	AND state_version = $3;

-- name: MarkVirtualChannelClosed :execrows
UPDATE virtual_channels
SET status = 'closed',
	state_version = state_version + 1,
	updated_at = $4,
	closed_at = $5
WHERE virtual_channel_id = $1
	AND status = $2
	AND state_version = $3;
