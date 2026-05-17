CREATE TABLE IF NOT EXISTS unroll_checkpoints (
    actor_id   TEXT PRIMARY KEY,
    state_type TEXT NOT NULL,
    state_data BLOB NOT NULL,
    version    BIGINT NOT NULL,
    updated_at BIGINT NOT NULL
);
