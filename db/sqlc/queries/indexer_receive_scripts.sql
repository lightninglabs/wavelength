-- name: UpsertIndexerReceiveScript :exec
INSERT INTO indexer_receive_scripts (
    principal_mailbox_id, pk_script, expires_at_unix_s, label, updated_at
) VALUES (
    $1, $2, $3, $4, $5
)
ON CONFLICT (principal_mailbox_id, pk_script) DO UPDATE SET
    expires_at_unix_s = excluded.expires_at_unix_s,
    label = excluded.label,
    updated_at = excluded.updated_at;

-- name: DeleteIndexerReceiveScript :execrows
DELETE FROM indexer_receive_scripts
WHERE principal_mailbox_id = $1
    AND pk_script = $2;

-- name: ListActiveIndexerReceiveScriptsByPrincipal :many
SELECT principal_mailbox_id, pk_script, expires_at_unix_s, label, updated_at
FROM indexer_receive_scripts
WHERE principal_mailbox_id = $1
    AND (expires_at_unix_s = 0 OR expires_at_unix_s >= $2)
ORDER BY pk_script ASC;

-- name: ListActiveIndexerReceivePrincipalsByScript :many
SELECT principal_mailbox_id, pk_script, expires_at_unix_s, label, updated_at
FROM indexer_receive_scripts
WHERE pk_script = $1
    AND (expires_at_unix_s = 0 OR expires_at_unix_s >= $2)
ORDER BY principal_mailbox_id ASC;
