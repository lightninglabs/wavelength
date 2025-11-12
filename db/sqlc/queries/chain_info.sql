-- name: GetChainInfo :one
SELECT * FROM chain_info WHERE chain_name = $1;

-- name: ListChainInfo :many
SELECT * FROM chain_info ORDER BY id;

-- name: UpsertChainInfo :exec
INSERT INTO chain_info (id, chain_name, genesis_hash) VALUES ($1, $2, $3)
ON CONFLICT (chain_name) DO UPDATE SET genesis_hash = $3;
