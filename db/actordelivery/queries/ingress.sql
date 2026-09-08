-- name: AdmitIngressReceipt :execrows
INSERT INTO ingress_receipts (id, payload_hash, mailbox_id, consumed_at, expires_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (id) DO UPDATE SET
    payload_hash = excluded.payload_hash,
    mailbox_id = excluded.mailbox_id,
    consumed_at = excluded.consumed_at,
    expires_at = excluded.expires_at
WHERE ingress_receipts.expires_at <= excluded.consumed_at;

-- name: GetIngressReceipt :one
SELECT id, payload_hash, mailbox_id, consumed_at, expires_at
FROM ingress_receipts WHERE id = $1;

-- name: PruneIngressReceipts :execrows
DELETE FROM ingress_receipts WHERE id IN (
    SELECT r.id FROM ingress_receipts r WHERE r.expires_at <= $1
    ORDER BY r.expires_at, r.id LIMIT $2
);

-- name: GetIngressQuarantine :one
SELECT id, lane, envelope, reason, attempts FROM ingress_quarantine WHERE id = $1;

-- name: ListIngressQuarantine :many
SELECT id, lane, envelope, reason, attempts FROM ingress_quarantine
WHERE lane = $1 ORDER BY created_at, id LIMIT 256;

-- name: InsertIngressQuarantine :execrows
INSERT INTO ingress_quarantine (id, lane, envelope, reason, created_at)
SELECT $1, $2, $3, $4, $5
WHERE (SELECT COUNT(*) FROM ingress_quarantine WHERE lane = $2) < 128
AND (SELECT COALESCE(SUM(length(envelope)), 0) FROM ingress_quarantine WHERE lane = $2) + CAST(sqlc.arg(envelope_bytes) AS BIGINT) <= 8388608
AND (SELECT COUNT(*) FROM ingress_quarantine) < 256
AND (SELECT COALESCE(SUM(length(envelope)), 0) FROM ingress_quarantine) + CAST(sqlc.arg(envelope_bytes) AS BIGINT) <= 16777216
ON CONFLICT (id) DO UPDATE SET id = excluded.id;

-- name: NoteIngressQuarantineAttempt :exec
UPDATE ingress_quarantine SET attempts = attempts + 1 WHERE id = $1;

-- name: DeleteIngressQuarantine :exec
DELETE FROM ingress_quarantine WHERE id = $1;
