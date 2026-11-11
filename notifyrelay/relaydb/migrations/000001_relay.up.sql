-- The relay owns a dedicated database. These records contain no wallet keys,
-- balances or mailbox application messages. BYTEA also stores byte slices in
-- SQLite; all binary values are bound as bytes by the generated queries.
CREATE TABLE relay_metadata (
    key TEXT PRIMARY KEY,
    value BYTEA NOT NULL CHECK (length(value) = 32)
);
CREATE TABLE relay_installations (
    id TEXT PRIMARY KEY,
    policy BYTEA NOT NULL,
    public_key BYTEA,
    attest_receipt BYTEA,
    assertion_counter BIGINT NOT NULL CHECK (assertion_counter BETWEEN 0 AND 4294967295),
    device_token TEXT NOT NULL,
    token_revision BIGINT NOT NULL CHECK (token_revision > 0),
    token_updated BIGINT NOT NULL,
    next_wake_at BIGINT NOT NULL DEFAULT 0,
    next_alert_at BIGINT NOT NULL DEFAULT 0,
    disabled BOOLEAN NOT NULL DEFAULT FALSE,
    deleted BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE TABLE relay_used_challenges (
    id TEXT PRIMARY KEY,
    expires_at BIGINT NOT NULL
);
CREATE INDEX relay_challenge_expiry ON relay_used_challenges (expires_at, id);
CREATE TABLE relay_subscriptions (
    id TEXT PRIMARY KEY,
    installation_id TEXT NOT NULL REFERENCES relay_installations(id),
    route_id TEXT NOT NULL,
    alerts_enabled BOOLEAN NOT NULL,
    presentation TEXT NOT NULL CHECK (presentation IN ('generic', 'receipt_hint')),
    expires_at BIGINT NOT NULL,
    revision BIGINT NOT NULL CHECK (revision > 0),
    UNIQUE (installation_id, route_id)
);
CREATE INDEX relay_subscription_expiry ON relay_subscriptions (expires_at, id);
CREATE TABLE relay_sources (
    id TEXT PRIMARY KEY,
    source_pubkey BYTEA NOT NULL CHECK (length(source_pubkey) = 33),
    subscriber_key BYTEA NOT NULL CHECK (length(subscriber_key) = 32),
    cursor BIGINT NOT NULL DEFAULT 0 CHECK (cursor >= 0),
    ack_pending BOOLEAN NOT NULL DEFAULT FALSE,
    claim_token TEXT NOT NULL DEFAULT '',
    lease_until BIGINT NOT NULL DEFAULT 0
);
CREATE TABLE relay_feed_bindings (
    source_id TEXT NOT NULL REFERENCES relay_sources(id),
    subscription_id TEXT NOT NULL REFERENCES relay_subscriptions(id) ON DELETE CASCADE,
    feed_token BYTEA NOT NULL CHECK (length(feed_token) = 32),
    event_classes INTEGER NOT NULL CHECK (event_classes BETWEEN 1 AND 3),
    expires_at BIGINT NOT NULL,
    last_generation BIGINT NOT NULL DEFAULT 0 CHECK (last_generation >= 0),
    PRIMARY KEY (source_id, subscription_id)
);
CREATE TABLE relay_jobs (
    id TEXT PRIMARY KEY,
    subscription_id TEXT NOT NULL REFERENCES relay_subscriptions(id) ON DELETE CASCADE,
    scope TEXT NOT NULL CHECK (scope IN ('wake', 'alert')),
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
CREATE INDEX relay_job_due ON relay_jobs (next_at, lease_until, id);
CREATE TABLE relay_dedup (
    subscription_id TEXT NOT NULL REFERENCES relay_subscriptions(id) ON DELETE CASCADE,
    installation_id TEXT NOT NULL REFERENCES relay_installations(id),
    scope TEXT NOT NULL CHECK (scope IN ('wake', 'alert')),
    event_hash TEXT NOT NULL,
    expires_at BIGINT NOT NULL,
    PRIMARY KEY (subscription_id, scope, event_hash)
);
CREATE INDEX relay_dedup_installation ON relay_dedup (installation_id);
CREATE INDEX relay_dedup_expiry ON relay_dedup (expires_at, subscription_id);
