-- Mailbox RPC client persistence queries.
--
-- These queries persist the minimal state needed to make mailboxrpcclient
-- crash-safe under cursor-based AckUpTo:
--   - The pull cursor (watermark), and
--   - response payloads keyed by correlation_id.

-- name: MailboxRPCClientGetCursor :one
-- Get the current cursor.
SELECT cursor FROM mailboxrpcclient_cursors
WHERE mailbox_id = $1;

-- name: MailboxRPCClientUpsertCursor :exec
-- Set cursor to the provided value, but only if it moves forward.
INSERT INTO mailboxrpcclient_cursors (mailbox_id, cursor)
VALUES ($1, $2)
ON CONFLICT (mailbox_id) DO UPDATE
SET cursor = excluded.cursor
WHERE excluded.cursor > mailboxrpcclient_cursors.cursor;

-- name: MailboxRPCClientPutResponse :exec
-- Store a response payload if it doesn't already exist.
INSERT INTO mailboxrpcclient_responses (mailbox_id, correlation_id, payload)
VALUES ($1, $2, $3)
ON CONFLICT (mailbox_id, correlation_id) DO NOTHING;

-- name: MailboxRPCClientGetResponse :one
-- Get a previously stored response payload.
SELECT payload FROM mailboxrpcclient_responses
WHERE mailbox_id = $1 AND correlation_id = $2;

-- name: MailboxRPCClientDeleteResponse :exec
-- Delete a stored response payload.
DELETE FROM mailboxrpcclient_responses
WHERE mailbox_id = $1 AND correlation_id = $2;

