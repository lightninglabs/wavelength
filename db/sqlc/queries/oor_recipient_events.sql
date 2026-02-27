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

-- name: ListOORRecipientEventsAfterWithSession :many
SELECT re.recipient_pk_script, re.event_id, s.session_id,
       re.output_index, re.value, re.created_at
FROM oor_recipient_events re
JOIN oor_sessions s ON s.id = re.session_db_id
WHERE re.recipient_pk_script = $1 AND re.event_id > $2
ORDER BY re.event_id ASC
LIMIT $3;

-- name: GetOORRecipientEventBySessionOutput :one
SELECT re.recipient_pk_script, re.event_id, re.session_db_id,
       re.output_index, re.value, re.created_at
FROM oor_recipient_events re
JOIN oor_sessions s ON s.id = re.session_db_id
WHERE re.recipient_pk_script = $1
    AND s.session_id = $2
    AND re.output_index = $3;
