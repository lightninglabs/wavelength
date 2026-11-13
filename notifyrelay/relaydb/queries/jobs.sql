-- name: GetJob :one
SELECT * FROM relay_jobs WHERE id = $1;

-- name: CountPendingJobs :one
SELECT COUNT(*) FROM relay_jobs;

-- name: CoalesceJob :exec
INSERT INTO relay_jobs (
    id, subscription_id, scope, generation, event_hash, next_at, expires_at
) VALUES ($1, $2, $3, 1, $4, $5, $6)
ON CONFLICT (subscription_id, scope) DO UPDATE SET
    generation = relay_jobs.generation + 1,
    event_hash = excluded.event_hash, expires_at = excluded.expires_at;

-- name: GetDedup :one
SELECT expires_at FROM relay_dedup
WHERE subscription_id = $1 AND scope = $2 AND event_hash = $3;

-- name: CountDedup :one
SELECT COUNT(*) FROM relay_dedup;

-- name: CountInstallationDedup :one
SELECT COUNT(*) FROM relay_dedup WHERE installation_id = $1;

-- name: CountSubscriptionDedup :one
SELECT COUNT(*) FROM relay_dedup WHERE subscription_id = $1;

-- name: PutDedup :exec
INSERT INTO relay_dedup (subscription_id, installation_id, scope, event_hash, expires_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (subscription_id, scope, event_hash) DO UPDATE
SET expires_at = excluded.expires_at;

-- name: ListDueJobs :many
SELECT j.* FROM relay_jobs AS j
JOIN relay_subscriptions AS s ON s.id = j.subscription_id
JOIN relay_installations AS i ON i.id = s.installation_id
WHERE j.next_at <= sqlc.arg(now_nano) AND j.lease_until <= sqlc.arg(now_nano)
    AND j.expires_at > sqlc.arg(now_nano)
    AND s.expires_at > sqlc.arg(now_unix)
    AND NOT i.disabled AND NOT i.deleted
    AND i.delivery_lease_until <= sqlc.arg(now_nano)
    AND (j.scope != 'alert' OR s.alerts_enabled)
    AND CASE j.scope WHEN 'wake' THEN i.next_wake_at
        WHEN 'round' THEN i.next_round_at
        WHEN 'schedule' THEN i.next_schedule_at ELSE i.next_alert_at END
        <= sqlc.arg(now_nano)
ORDER BY CASE WHEN j.scope = 'round' THEN 0 ELSE 1 END,
    j.next_at, j.id LIMIT sqlc.arg(batch_limit);

-- name: ClaimInstallation :execrows
UPDATE relay_installations
SET delivery_claim_token = sqlc.arg(claim_token),
    delivery_lease_until = sqlc.arg(lease_until)
WHERE id = sqlc.arg(id) AND delivery_lease_until <= sqlc.arg(now_nano)
    AND NOT disabled AND NOT deleted;

-- name: ClaimJob :one
UPDATE relay_jobs SET claim_token = sqlc.arg(claim_token),
    lease_until = sqlc.arg(lease_until), attempts = attempts + 1
WHERE id = sqlc.arg(id) AND lease_until <= sqlc.arg(now_nano)
RETURNING *;

-- name: ReleaseInstallation :execrows
UPDATE relay_installations SET delivery_claim_token = '', delivery_lease_until = 0
WHERE id = $1 AND delivery_claim_token = $2;

-- name: FinishJob :execrows
UPDATE relay_jobs SET attempts = CASE
        WHEN sqlc.arg(done_generation) > done_generation THEN 0 ELSE attempts END,
    done_generation = sqlc.arg(done_generation),
    next_at = sqlc.arg(next_at), claim_token = '', lease_until = 0
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(claim_token);

-- name: DeleteClaimedJob :execrows
DELETE FROM relay_jobs WHERE id = $1 AND claim_token = $2;

-- name: PruneExpiredJobs :execrows
DELETE FROM relay_jobs WHERE id IN (
    SELECT j.id FROM relay_jobs AS j WHERE j.expires_at <= $1
    ORDER BY j.expires_at, j.id LIMIT $2
);

-- name: PruneExpiredDedup :execrows
DELETE FROM relay_dedup WHERE (subscription_id, scope, event_hash) IN (
    SELECT d.subscription_id, d.scope, d.event_hash FROM relay_dedup AS d
    WHERE d.expires_at <= $1 ORDER BY d.expires_at, d.subscription_id LIMIT $2
);

-- name: DeferUnclaimedJob :exec
UPDATE relay_jobs SET next_at = sqlc.arg(next_at)
WHERE id = sqlc.arg(id) AND lease_until <= sqlc.arg(now_nano);

-- name: SetRoundJobWindow :exec
UPDATE relay_jobs SET next_at = $2, expires_at = $3
WHERE id = $1 AND lease_until = 0;
