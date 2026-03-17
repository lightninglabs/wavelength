-- name: AppendMailboxEnvelope :one
INSERT INTO mailbox_envelopes (
    recipient, msg_id, envelope, created_at
) VALUES (
    $1, $2, $3, $4
)
ON CONFLICT (recipient, msg_id) DO NOTHING
RETURNING event_seq;

-- name: PullMailboxEnvelopes :many
SELECT event_seq, recipient, msg_id, envelope, created_at
FROM mailbox_envelopes
WHERE recipient = $1
  AND event_seq >= $2
ORDER BY event_seq ASC
LIMIT $3;

-- name: UpsertMailboxAckCursor :exec
INSERT INTO mailbox_ack_cursors (recipient, ack_cursor)
VALUES ($1, $2)
ON CONFLICT (recipient) DO UPDATE
    SET ack_cursor = CASE
        WHEN excluded.ack_cursor > mailbox_ack_cursors.ack_cursor
            THEN excluded.ack_cursor
        ELSE mailbox_ack_cursors.ack_cursor
    END;

-- name: GetMailboxAckCursor :one
SELECT ack_cursor
FROM mailbox_ack_cursors
WHERE recipient = $1;

-- name: CountMailboxEnvelopes :one
SELECT COUNT(*) AS cnt
FROM mailbox_envelopes
WHERE recipient = $1;

-- name: DeleteAckedMailboxEnvelopes :execrows
DELETE FROM mailbox_envelopes
WHERE recipient = $1
  AND event_seq < $2;
