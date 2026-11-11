-- name: GetFeedBinding :one
SELECT * FROM relay_feed_bindings WHERE source_id = $1 AND subscription_id = $2;

-- name: PutFeedBinding :exec
INSERT INTO relay_feed_bindings (
    source_id, subscription_id, feed_token, event_classes, expires_at
) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (source_id, subscription_id) DO UPDATE SET
    feed_token = excluded.feed_token, event_classes = excluded.event_classes,
    expires_at = excluded.expires_at,
    last_generation = CASE WHEN relay_feed_bindings.feed_token = excluded.feed_token
        THEN relay_feed_bindings.last_generation ELSE 0 END;

-- name: AdvanceFeedBinding :execrows
UPDATE relay_feed_bindings SET last_generation = sqlc.arg(generation)
WHERE source_id = sqlc.arg(source_id) AND subscription_id = sqlc.arg(subscription_id)
    AND feed_token = sqlc.arg(feed_token) AND last_generation < sqlc.arg(generation)
    AND expires_at > sqlc.arg(now_unix);
