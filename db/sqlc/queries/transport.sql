-- name: GetMailboxIngressCursor :one
SELECT local_mailbox_id, remote_mailbox_id, pull_cursor,
    dispatch_committed_to, ack_target, ack_committed_to, last_pull_at,
    last_dispatch_at, last_ack_at, last_error, created_at, updated_at
FROM mailbox_ingress_cursors
WHERE local_mailbox_id = $1;

-- name: UpsertMailboxIngressCursor :exec
INSERT INTO mailbox_ingress_cursors (
    local_mailbox_id, remote_mailbox_id, pull_cursor,
    dispatch_committed_to, ack_target, ack_committed_to,
    last_pull_at, last_dispatch_at, last_ack_at, last_error,
    created_at, updated_at
) VALUES (
    $1, $2, $3,
    $4, $5, $6,
    $7, $8, $9, $10,
    $11, $12
)
ON CONFLICT (local_mailbox_id) DO UPDATE SET
    remote_mailbox_id = excluded.remote_mailbox_id,
    pull_cursor = excluded.pull_cursor,
    dispatch_committed_to = excluded.dispatch_committed_to,
    ack_target = excluded.ack_target,
    ack_committed_to = excluded.ack_committed_to,
    last_pull_at = excluded.last_pull_at,
    last_dispatch_at = excluded.last_dispatch_at,
    last_ack_at = excluded.last_ack_at,
    last_error = excluded.last_error,
    updated_at = excluded.updated_at;

-- name: InsertMailboxEgress :exec
INSERT INTO mailbox_egress (
    id, connector, local_mailbox_id, remote_mailbox_id,
    rpc_kind, service, method, correlation_id, reply_to,
    msg_id, idempotency_key, envelope, status, attempts,
    max_attempts, next_attempt_at, created_at, updated_at
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8, $9,
    $10, $11, $12, 'pending', 0,
    $13, $14, $15, $15
)
ON CONFLICT (connector, local_mailbox_id, idempotency_key) DO UPDATE SET
    remote_mailbox_id = excluded.remote_mailbox_id,
    rpc_kind = excluded.rpc_kind,
    service = excluded.service,
    method = excluded.method,
    correlation_id = excluded.correlation_id,
    reply_to = excluded.reply_to,
    msg_id = excluded.msg_id,
    envelope = excluded.envelope,
    status = 'pending',
    attempts = 0,
    max_attempts = excluded.max_attempts,
    next_attempt_at = excluded.next_attempt_at,
    claim_owner = NULL,
    claim_token = NULL,
    claim_until = NULL,
    last_error = NULL,
    sent_at = NULL,
    updated_at = excluded.updated_at
WHERE mailbox_egress.status = 'sent';

-- name: ListDueMailboxEgressIDs :many
SELECT id
FROM mailbox_egress
WHERE next_attempt_at <= $1
  AND attempts < max_attempts
  AND (
    status = 'pending' OR
    (status = 'claimed' AND claim_until <= $1)
  )
ORDER BY next_attempt_at, created_at, id
LIMIT $2;

-- name: ClaimMailboxEgress :one
UPDATE mailbox_egress
SET status = 'claimed',
    claim_owner = $2,
    claim_token = $3,
    claim_until = $4,
    attempts = attempts + 1,
    updated_at = $5
WHERE id = $1
  AND next_attempt_at <= $5
  AND attempts < max_attempts
  AND (
    status = 'pending' OR
    (status = 'claimed' AND claim_until <= $5)
  )
RETURNING id, connector, local_mailbox_id, remote_mailbox_id,
    rpc_kind, service, method, correlation_id, reply_to, msg_id,
    idempotency_key, envelope, status, attempts, max_attempts,
    next_attempt_at, claim_owner, claim_token, claim_until,
    last_error, created_at, updated_at, sent_at;

-- name: MarkMailboxEgressSent :exec
UPDATE mailbox_egress
SET status = 'sent',
    sent_at = $3,
    updated_at = $3,
    claim_owner = NULL,
    claim_token = NULL,
    claim_until = NULL,
    last_error = NULL
WHERE id = $1
  AND claim_token = $2
  AND status = 'claimed';

-- name: ReleaseMailboxEgressForRetry :exec
UPDATE mailbox_egress
SET status = CASE
        WHEN attempts >= max_attempts THEN 'dead'
        ELSE 'pending'
    END,
    next_attempt_at = $3,
    updated_at = $4,
    claim_owner = NULL,
    claim_token = NULL,
    claim_until = NULL,
    last_error = $5
WHERE id = $1
  AND claim_token = $2
  AND status = 'claimed';

-- name: ReleaseExpiredMailboxEgressClaims :exec
UPDATE mailbox_egress
SET status = 'pending',
    claim_owner = NULL,
    claim_token = NULL,
    claim_until = NULL,
    updated_at = $2
WHERE status = 'claimed'
  AND claim_until <= $1;
