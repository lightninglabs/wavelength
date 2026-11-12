-- name: GetMetadata :one
SELECT value FROM relay_metadata WHERE key = $1;

-- name: InsertMetadata :execrows
INSERT INTO relay_metadata (key, value) VALUES ($1, $2)
ON CONFLICT (key) DO NOTHING;

-- name: GetInstallation :one
SELECT * FROM relay_installations WHERE id = $1;

-- name: PutInstallation :execrows
INSERT INTO relay_installations (
    id, policy, public_key, attest_receipt, assertion_counter, device_token,
    token_revision, token_updated, next_wake_at, next_alert_at, disabled, deleted
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (id) DO UPDATE SET
    policy = excluded.policy, public_key = excluded.public_key,
    attest_receipt = excluded.attest_receipt,
    assertion_counter = excluded.assertion_counter,
    device_token = excluded.device_token, token_revision = excluded.token_revision,
    token_updated = excluded.token_updated, next_wake_at = excluded.next_wake_at,
    next_alert_at = excluded.next_alert_at, disabled = excluded.disabled,
    deleted = excluded.deleted
WHERE excluded.assertion_counter >= relay_installations.assertion_counter
    AND excluded.token_revision >= relay_installations.token_revision;

-- name: GetConsumedChallenge :one
SELECT expires_at FROM relay_used_challenges WHERE id = $1;

-- name: ConsumeChallenge :execrows
INSERT INTO relay_used_challenges (id, expires_at) VALUES ($1, $2)
ON CONFLICT (id) DO NOTHING;

-- name: CountConsumedChallenges :one
SELECT COUNT(*) FROM relay_used_challenges;

-- name: PruneConsumedChallenges :execrows
DELETE FROM relay_used_challenges WHERE id IN (
    SELECT c.id FROM relay_used_challenges AS c WHERE c.expires_at <= $1
    ORDER BY c.expires_at, c.id LIMIT $2
);

-- name: GetSubscription :one
SELECT * FROM relay_subscriptions WHERE id = $1;

-- name: GetSubscriptionByRoute :one
SELECT * FROM relay_subscriptions WHERE installation_id = $1 AND route_id = $2;

-- name: CountInstallationSubscriptions :one
SELECT COUNT(*) FROM relay_subscriptions WHERE installation_id = $1;

-- name: PutSubscription :execrows
INSERT INTO relay_subscriptions (
    id, installation_id, route_id, alerts_enabled, presentation, expires_at, revision
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (id) DO UPDATE SET
    alerts_enabled = excluded.alerts_enabled, presentation = excluded.presentation,
    expires_at = excluded.expires_at, revision = excluded.revision
WHERE relay_subscriptions.installation_id = excluded.installation_id
    AND relay_subscriptions.route_id = excluded.route_id
    AND excluded.revision >= relay_subscriptions.revision;

-- name: DeleteSubscription :exec
DELETE FROM relay_subscriptions WHERE id = $1 AND installation_id = $2;

-- name: ListInstallationSubscriptions :many
SELECT * FROM relay_subscriptions WHERE installation_id = $1 ORDER BY id;

-- name: ListExpiredSubscriptions :many
SELECT * FROM relay_subscriptions WHERE expires_at <= $1
ORDER BY expires_at, id LIMIT $2;

-- name: GetSource :one
SELECT * FROM relay_sources WHERE id = $1;

-- name: InsertSource :execrows
INSERT INTO relay_sources (id, source_pubkey, subscriber_key) VALUES ($1, $2, $3)
ON CONFLICT (id) DO NOTHING;
