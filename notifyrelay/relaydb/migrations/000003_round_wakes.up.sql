-- Preserve existing jobs and routes while adding opt-in round wakes.
CREATE TABLE relay_feed_bindings_round (
    source_id TEXT NOT NULL REFERENCES relay_sources(id),
    subscription_id TEXT NOT NULL REFERENCES relay_subscriptions(id) ON DELETE CASCADE,
    feed_token BYTEA NOT NULL CHECK (length(feed_token) = 32),
    event_classes INTEGER NOT NULL CHECK (event_classes BETWEEN 1 AND 7),
    expires_at BIGINT NOT NULL,
    last_generation BIGINT NOT NULL DEFAULT 0 CHECK (last_generation >= 0),
    PRIMARY KEY (source_id, subscription_id)
);

INSERT INTO relay_feed_bindings_round SELECT * FROM relay_feed_bindings;

DROP TABLE relay_feed_bindings;

ALTER TABLE relay_feed_bindings_round RENAME TO relay_feed_bindings;

CREATE TABLE relay_jobs_round (
    id TEXT PRIMARY KEY,
    subscription_id TEXT NOT NULL REFERENCES relay_subscriptions(id) ON DELETE CASCADE,
    scope TEXT NOT NULL CHECK (scope IN ('wake', 'alert', 'round', 'schedule')),
    generation BIGINT NOT NULL CHECK (generation > 0),
    done_generation BIGINT NOT NULL DEFAULT 0,
    event_hash TEXT NOT NULL,
    next_at BIGINT NOT NULL,
    expires_at BIGINT NOT NULL,
    claim_token TEXT NOT NULL DEFAULT '',
    lease_until BIGINT NOT NULL DEFAULT 0,
    attempts INTEGER NOT NULL DEFAULT 0,
    UNIQUE (subscription_id, scope)
);

INSERT INTO relay_jobs_round SELECT * FROM relay_jobs;

DROP TABLE relay_jobs;

ALTER TABLE relay_jobs_round RENAME TO relay_jobs;

CREATE INDEX relay_job_due ON relay_jobs (next_at, lease_until, id);

CREATE INDEX relay_job_expiry ON relay_jobs (expires_at, id);

ALTER TABLE relay_installations ADD COLUMN next_round_at BIGINT NOT NULL DEFAULT 0;

ALTER TABLE relay_installations ADD COLUMN next_schedule_at BIGINT NOT NULL DEFAULT 0;

ALTER TABLE relay_feed_bindings ADD COLUMN scheduled_cutoff BIGINT NOT NULL DEFAULT 0;
