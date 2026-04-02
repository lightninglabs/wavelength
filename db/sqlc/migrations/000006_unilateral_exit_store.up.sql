-- unilateral_exit_jobs stores manager-facing control-plane state for one
-- unroll job per target outpoint.
CREATE TABLE IF NOT EXISTS unilateral_exit_jobs (
    -- target_outpoint_hash identifies the target transaction.
    target_outpoint_hash BLOB NOT NULL,

    -- target_outpoint_index identifies the target output index.
    target_outpoint_index INTEGER NOT NULL CHECK (
        target_outpoint_index >= 0
    ),

    -- actor_id is the durable actor mailbox id for this target job.
    actor_id TEXT NOT NULL,

    -- status is the control-plane job status:
    --   0 = pending
    --   1 = materializing
    --   2 = csv_pending
    --   3 = sweeping
    --   4 = completed
    --   5 = failed
    status INTEGER NOT NULL,

    -- trigger identifies what started the job:
    --   0 = manual
    --   1 = critical_expiry
    --   2 = restart
    --   3 = fraud_spend
    trigger INTEGER NOT NULL,

    -- last_error stores the latest terminal or diagnostic error string.
    last_error TEXT,

    -- created_at is the unix timestamp when the row was first written.
    created_at BIGINT NOT NULL,

    -- updated_at is the unix timestamp of the latest row update.
    updated_at BIGINT NOT NULL,

    PRIMARY KEY (target_outpoint_hash, target_outpoint_index)
);

CREATE INDEX IF NOT EXISTS idx_unilateral_exit_jobs_status_updated
    ON unilateral_exit_jobs(status, updated_at DESC);
