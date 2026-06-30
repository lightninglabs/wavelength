-- Macaroon root key store queries.

-- name: GetMacaroonRootKey :one
SELECT * FROM macaroons
WHERE id = $1;

-- name: InsertMacaroonRootKey :exec
INSERT INTO macaroons (id, root_key)
VALUES ($1, $2);
