-- name: GetServiceOperation :one
SELECT * FROM service_operations WHERE operation_id = $1;

-- name: SaveServiceOperation :exec
INSERT INTO service_operations (operation_id, authorization_blob)
VALUES ($1, $2)
ON CONFLICT (operation_id) DO UPDATE SET
    authorization_blob = excluded.authorization_blob, round_id = NULL, deadline_unix = 0;

-- name: InsertServiceOperationInput :exec
INSERT INTO service_operation_inputs (operation_id, txid, output_index, is_forfeit)
VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING;

-- name: ListServiceOperationInputs :many
SELECT txid, output_index, is_forfeit FROM service_operation_inputs
WHERE operation_id = $1 ORDER BY txid, output_index;

-- name: OtherServiceInputOwners :many
SELECT i.operation_id FROM service_operation_inputs AS i
JOIN service_operations AS o ON o.operation_id = i.operation_id
WHERE i.txid = $1 AND i.output_index = $2 AND o.active = TRUE
AND i.operation_id != $3 AND NOT EXISTS (
    SELECT 1 FROM rounds AS r WHERE r.round_id = o.round_id
    AND r.status IN ('confirmed', 'failed')
);

-- name: HasServiceInputOwner :one
SELECT EXISTS (SELECT 1 FROM service_operation_inputs AS i
JOIN service_operations AS o ON o.operation_id = i.operation_id
WHERE i.txid = $1 AND i.output_index = $2 AND o.active = TRUE
AND NOT EXISTS (SELECT 1 FROM rounds AS r WHERE r.round_id = o.round_id
    AND r.status IN ('confirmed', 'failed')));

-- name: BindServiceOperation :execrows
UPDATE service_operations SET round_id = $1, deadline_unix = $2
WHERE operation_id = $3 AND active = TRUE;

-- name: ListDeferredServiceOperations :many
SELECT o.* FROM service_operations AS o
WHERE o.active = TRUE AND NOT EXISTS (
    SELECT 1 FROM rounds AS r WHERE r.round_id = o.round_id
) ORDER BY operation_id;

-- name: CompleteServiceOperation :exec
UPDATE service_operations SET active = FALSE WHERE operation_id = $1;
