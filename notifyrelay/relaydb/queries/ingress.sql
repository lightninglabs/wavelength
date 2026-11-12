-- name: ClaimSource :one
UPDATE relay_sources SET claim_token = sqlc.arg(claim_token),
    lease_until = sqlc.arg(lease_until)
WHERE id = sqlc.arg(id) AND lease_until <= sqlc.arg(now_nano)
RETURNING *;

-- name: GetClaimedSource :one
SELECT * FROM relay_sources
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(claim_token)
    AND lease_until > sqlc.arg(now_nano);

-- name: CommitSourceCursor :execrows
UPDATE relay_sources SET cursor = sqlc.arg(next_cursor), ack_pending = TRUE
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(claim_token)
    AND lease_until > sqlc.arg(now_nano) AND cursor = sqlc.arg(prior_cursor)
    AND cursor < sqlc.arg(next_cursor);

-- name: ClearSourceAck :execrows
UPDATE relay_sources SET ack_pending = FALSE
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(claim_token)
    AND lease_until > sqlc.arg(now_nano) AND cursor = sqlc.arg(cursor);

-- name: ReleaseSource :exec
UPDATE relay_sources SET claim_token = '', lease_until = 0
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(claim_token);
