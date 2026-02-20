-- name: GetMaxOORRecipientEventID :one
SELECT CAST(COALESCE(MAX(event_id), 0) AS BIGINT) FROM oor_recipient_events
WHERE recipient_pk_script = $1;

-- name: InsertOORRecipientEvent :execrows
INSERT INTO oor_recipient_events (
    recipient_pk_script, event_id, session_db_id, output_index, value,
    created_at
) VALUES (
    $1, $2, $3, $4, $5, $6
)
ON CONFLICT DO NOTHING;

-- name: ListOORRecipientEventsAfter :many
SELECT recipient_pk_script, event_id, session_db_id, output_index, value,
       created_at
FROM oor_recipient_events
WHERE recipient_pk_script = $1 AND event_id > $2
ORDER BY event_id ASC
LIMIT $3;
