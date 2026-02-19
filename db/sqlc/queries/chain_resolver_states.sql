-- Chain resolver state persistence queries.

-- name: UpsertChainResolverState :exec
INSERT INTO chain_resolver_states (
    outpoint_hash, outpoint_index, state, state_details,
    created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6
)
ON CONFLICT (outpoint_hash, outpoint_index) DO UPDATE SET
    state = EXCLUDED.state,
    state_details = EXCLUDED.state_details,
    updated_at = EXCLUDED.updated_at;

-- name: GetChainResolverState :one
SELECT * FROM chain_resolver_states
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: ListActiveChainResolverStates :many
SELECT * FROM chain_resolver_states
WHERE state NOT IN ('resolved', 'failed')
ORDER BY created_at ASC;

-- name: DeleteChainResolverState :exec
DELETE FROM chain_resolver_states
WHERE outpoint_hash = $1 AND outpoint_index = $2;
