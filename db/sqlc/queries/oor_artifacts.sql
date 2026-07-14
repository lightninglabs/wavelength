-- OOR artifact store queries.

-- name: UpsertOORPackage :execrows
INSERT INTO oor_packages (
    session_id, direction, ark_psbt, created_at, updated_at,
    taproot_asset_transfer
) VALUES (
    $1, $2, $3, $4, $5, $6
)
ON CONFLICT (session_id) DO UPDATE SET
    ark_psbt = EXCLUDED.ark_psbt,
    taproot_asset_transfer = EXCLUDED.taproot_asset_transfer,
    updated_at = EXCLUDED.updated_at
WHERE oor_packages.direction = EXCLUDED.direction;

-- name: GetOORPackage :one
SELECT * FROM oor_packages
WHERE session_id = $1;

-- name: ListOORPackagesByDirection :many
SELECT * FROM oor_packages
WHERE direction = $1
ORDER BY updated_at DESC;

-- name: ListOORPackages :many
SELECT * FROM oor_packages
ORDER BY updated_at DESC;

-- name: DeleteOORPackageCheckpoints :exec
DELETE FROM oor_package_checkpoints
WHERE session_id = $1;

-- name: InsertOORPackageCheckpoint :exec
INSERT INTO oor_package_checkpoints (
    session_id, checkpoint_index, checkpoint_psbt, created_at
) VALUES (
    $1, $2, $3, $4
)
ON CONFLICT (session_id, checkpoint_index) DO UPDATE SET
    checkpoint_psbt = EXCLUDED.checkpoint_psbt;

-- name: ListOORPackageCheckpoints :many
SELECT * FROM oor_package_checkpoints
WHERE session_id = $1
ORDER BY checkpoint_index ASC;

-- name: UpsertOORVTXOBinding :execrows
INSERT INTO oor_vtxo_bindings (
    outpoint_hash, outpoint_index, session_id, output_index, link_kind,
    created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
)
ON CONFLICT (outpoint_hash, outpoint_index, link_kind) DO UPDATE SET
    updated_at = EXCLUDED.updated_at
WHERE oor_vtxo_bindings.session_id = EXCLUDED.session_id
    AND oor_vtxo_bindings.output_index = EXCLUDED.output_index;

-- name: GetOORVTXOBindingByOutpoint :one
SELECT * FROM oor_vtxo_bindings
WHERE outpoint_hash = $1 AND outpoint_index = $2
ORDER BY link_kind ASC, output_index ASC
LIMIT 1;

-- name: GetOORVTXOBindingByOutpointAndKind :one
SELECT * FROM oor_vtxo_bindings
WHERE outpoint_hash = $1
    AND outpoint_index = $2
    AND link_kind = $3;

-- name: ListOORVTXOBindingsBySession :many
SELECT
    b.outpoint_hash,
    b.outpoint_index,
    b.session_id,
    b.output_index,
    b.link_kind,
    v.pk_script AS recipient_pk_script,
    v.amount AS value_sat,
    b.created_at,
    b.updated_at
FROM oor_vtxo_bindings b
JOIN vtxos v ON v.outpoint_hash = b.outpoint_hash
    AND v.outpoint_index = b.outpoint_index
WHERE b.session_id = $1
ORDER BY b.link_kind, b.output_index ASC;

-- name: GetOORPackageByOutpoint :one
SELECT
    p.session_id,
    p.direction,
    p.ark_psbt,
    p.taproot_asset_transfer,
    p.created_at AS package_created_at,
    p.updated_at AS package_updated_at,
    b.outpoint_hash,
    b.outpoint_index,
    b.output_index,
    b.link_kind,
    v.pk_script AS recipient_pk_script,
    v.amount AS value_sat,
    b.created_at AS binding_created_at,
    b.updated_at AS binding_updated_at
FROM oor_vtxo_bindings b
JOIN oor_packages p ON p.session_id = b.session_id
JOIN vtxos v ON v.outpoint_hash = b.outpoint_hash
    AND v.outpoint_index = b.outpoint_index
WHERE b.outpoint_hash = $1 AND b.outpoint_index = $2
ORDER BY b.link_kind ASC, b.output_index ASC
LIMIT 1;

-- name: GetOORPackageByOutpointAndKind :one
SELECT
    p.session_id,
    p.direction,
    p.ark_psbt,
    p.taproot_asset_transfer,
    p.created_at AS package_created_at,
    p.updated_at AS package_updated_at,
    b.outpoint_hash,
    b.outpoint_index,
    b.output_index,
    b.link_kind,
    v.pk_script AS recipient_pk_script,
    v.amount AS value_sat,
    b.created_at AS binding_created_at,
    b.updated_at AS binding_updated_at
FROM oor_vtxo_bindings b
JOIN oor_packages p ON p.session_id = b.session_id
JOIN vtxos v ON v.outpoint_hash = b.outpoint_hash
    AND v.outpoint_index = b.outpoint_index
WHERE b.outpoint_hash = $1
    AND b.outpoint_index = $2
    AND b.link_kind = $3;

-- name: UpsertOORRecipientCursor :exec
INSERT INTO oor_recipient_cursors (
    recipient_pk_script, last_event_id, updated_at, last_session_id
) VALUES (
    $1, $2, $3, $4
)
ON CONFLICT (recipient_pk_script) DO UPDATE SET
    last_event_id = EXCLUDED.last_event_id,
    updated_at = EXCLUDED.updated_at,
    last_session_id = EXCLUDED.last_session_id;

-- name: GetOORRecipientCursor :one
SELECT * FROM oor_recipient_cursors
WHERE recipient_pk_script = $1;

-- name: ListOORRecipientCursors :many
SELECT * FROM oor_recipient_cursors
ORDER BY updated_at DESC;

-- name: UpsertOwnedReceiveScript :exec
INSERT INTO owned_receive_scripts (
    pk_script, client_key_id, operator_pubkey, exit_delay, source,
    created_at, last_used_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
)
ON CONFLICT (pk_script) DO UPDATE SET
    client_key_id = EXCLUDED.client_key_id,
    operator_pubkey = EXCLUDED.operator_pubkey,
    exit_delay = EXCLUDED.exit_delay,
    source = EXCLUDED.source,
    last_used_at = EXCLUDED.last_used_at;

-- name: GetOwnedReceiveScript :one
SELECT * FROM owned_receive_scripts
WHERE pk_script = $1;

-- name: ListOwnedReceiveScripts :many
SELECT * FROM owned_receive_scripts
ORDER BY created_at DESC;
