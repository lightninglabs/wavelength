-- name: ListOORStatus :many
-- Bound both ordered sources before the final merge, including SQLite where
-- an outer LIMIT alone would sort the entire UNION result.
WITH registry AS (
SELECT r.* FROM oor_registry_status r
WHERE r.created_at <= sqlc.arg(before_created_at)
    AND (r.created_at < sqlc.arg(before_created_at) OR r.session_id < sqlc.arg(before_id))
    AND (CAST(sqlc.arg(direction_filter) AS INTEGER) = 0 OR r.direction = sqlc.arg(direction_filter))
    AND (CAST(sqlc.arg(status_filter) AS INTEGER) = -1 OR r.status = sqlc.arg(status_filter))
ORDER BY r.created_at DESC, r.session_id DESC
LIMIT CAST(sqlc.arg(page_limit) AS BIGINT)
), package_floor AS (
-- A full registry page already outranks every older package. Keep timestamp
-- ties for the ID tie-breaker; a partial page must still search older history.
SELECT CASE WHEN COUNT(*) = sqlc.arg(page_limit) THEN MIN(created_at)
    ELSE CAST(-9223372036854775808 AS BIGINT) END AS created_at FROM registry
), packages AS (
SELECT p.* FROM oor_package_status p
WHERE p.created_at >= (SELECT created_at FROM package_floor)
    AND p.created_at <= sqlc.arg(before_created_at)
    AND (p.created_at < sqlc.arg(before_created_at) OR p.session_id < sqlc.arg(before_id))
    AND (CAST(sqlc.arg(direction_filter) AS INTEGER) = 0 OR p.direction = sqlc.arg(direction_filter))
    AND (CAST(sqlc.arg(status_filter) AS INTEGER) = -1 OR p.status = sqlc.arg(status_filter))
ORDER BY p.created_at DESC, p.session_id DESC
LIMIT CAST(sqlc.arg(page_limit) AS BIGINT)
)
SELECT * FROM packages
UNION ALL
SELECT * FROM registry
ORDER BY created_at DESC, session_id DESC
LIMIT CAST(sqlc.arg(page_limit) AS BIGINT);

-- name: GetOORStatus :one
SELECT session_id, direction, status, phase, last_error,
    created_at, updated_at, has_package
FROM oor_status
WHERE session_id = $1;

-- name: GetOOROutgoingStatusSnapshot :one
-- Incoming snapshots cannot contribute outgoing input or retry diagnostics.
SELECT * FROM oor_session_registry
WHERE session_id = $1 AND direction = 1;

-- name: GetOORDiagnosticPackage :one
-- A stored package may be a local transfer even if its registry row is gone.
SELECT p.direction,
    (EXISTS (SELECT 1 FROM oor_session_registry r WHERE r.session_id = p.session_id) OR
     EXISTS (SELECT 1 FROM oor_dispatch_attempts d WHERE d.session_id = p.session_id) OR
     EXISTS (SELECT 1 FROM oor_vtxo_bindings b WHERE b.session_id = p.session_id) OR
     EXISTS (SELECT 1 FROM vtxos v WHERE v.outpoint_hash = p.session_id) OR
     EXISTS (SELECT 1 FROM ledger_entries l WHERE l.session_id = p.session_id)) AS wallet_evidence
FROM oor_packages p
WHERE p.session_id = $1;
