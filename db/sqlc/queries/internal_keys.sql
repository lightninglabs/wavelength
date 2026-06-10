-- Internal key registry queries.

-- name: UpsertInternalKey :one
-- Register an internal key and return the stored row's id in a single
-- round-trip on both backends. ON CONFLICT (pubkey, key_family, key_index)
-- makes re-registration of an already-known triple idempotent; the no-op DO
-- UPDATE (rather than DO NOTHING) is required so RETURNING still fires on
-- conflict and the caller gets the existing id, closing the read-then-insert
-- race a separate re-select would leave open.
INSERT INTO internal_keys (
    pubkey, key_family, key_index, created_at
) VALUES ($1, $2, $3, $4)
ON CONFLICT (pubkey, key_family, key_index) DO UPDATE SET pubkey = excluded.pubkey
RETURNING id;

-- name: GetInternalKeyByID :one
SELECT * FROM internal_keys
WHERE id = $1;
