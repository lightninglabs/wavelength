-- name: GetFeedBinding :one
SELECT * FROM relay_feed_bindings WHERE source_id = $1 AND subscription_id = $2;

-- name: PutFeedBinding :execrows
INSERT INTO relay_feed_bindings (
    source_id, subscription_id, feed_token, event_classes, expires_at
) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (source_id, subscription_id) DO UPDATE SET
    feed_token = excluded.feed_token, event_classes = excluded.event_classes,
    expires_at = excluded.expires_at,
    last_generation = CASE WHEN relay_feed_bindings.feed_token = excluded.feed_token
        THEN relay_feed_bindings.last_generation ELSE 0 END
WHERE relay_feed_bindings.expires_at > 0;

-- A zero expiry is terminal for this source and subscription.
-- name: RevokeFeedBinding :exec
INSERT INTO relay_feed_bindings (
    source_id, subscription_id, feed_token, event_classes, expires_at
) VALUES ($1, $2, $3, 1, 0)
ON CONFLICT (source_id, subscription_id) DO UPDATE SET expires_at = 0;

-- Jobs coalesce per subscription and scope. Keep jobs that a second live
-- binding can still authorize.
-- name: DeleteRevokedFeedJobs :exec
DELETE FROM relay_jobs
WHERE relay_jobs.subscription_id = sqlc.arg(subscription_id)
    AND NOT EXISTS (
        SELECT 1 FROM relay_feed_bindings AS b
        WHERE b.subscription_id = relay_jobs.subscription_id
            AND b.expires_at > sqlc.arg(now_unix)
            AND (b.event_classes & CASE relay_jobs.scope
                WHEN 'wake' THEN 1
                WHEN 'alert' THEN 2
                WHEN 'round' THEN 4
                WHEN 'schedule' THEN 4
            END) != 0
    );

-- name: AdvanceFeedBinding :execrows
UPDATE relay_feed_bindings SET last_generation = sqlc.arg(generation)
WHERE source_id = sqlc.arg(source_id) AND subscription_id = sqlc.arg(subscription_id)
    AND feed_token = sqlc.arg(feed_token) AND last_generation < sqlc.arg(generation)
    AND expires_at > sqlc.arg(now_unix);

-- name: ListScheduledBindings :many
SELECT b.* FROM relay_feed_bindings AS b
JOIN relay_subscriptions AS s ON s.id = b.subscription_id
WHERE b.source_id = $1 AND (b.event_classes & 4) != 0
    AND b.expires_at > $2 AND s.expires_at > $2
    AND b.scheduled_cutoff < $3
ORDER BY b.subscription_id LIMIT 64;

-- name: MarkScheduledBinding :execrows
UPDATE relay_feed_bindings SET scheduled_cutoff = $3
WHERE source_id = $1 AND subscription_id = $2 AND scheduled_cutoff < $3;
