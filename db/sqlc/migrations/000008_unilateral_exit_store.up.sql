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
    --   3 = sweeping (sweep broadcast, awaiting confirmation)
    --   4 = completed
    --   5 = failed
    --   6 = sweep_broadcasting (sweep built, not yet submitted)
    status INTEGER NOT NULL,

    -- trigger identifies what started the job:
    --   0 = manual
    --   1 = critical_expiry
    --   2 = restart
    --   3 = fraud_spend
    trigger INTEGER NOT NULL,

    -- last_error stores the latest terminal or diagnostic error string.
    last_error TEXT,

    -- sweep_txid is the 32-byte txid of the final sweep transaction.
    -- NULL until the sweep is broadcast.
    sweep_txid BLOB,

    -- created_at is the unix timestamp when the row was first written.
    created_at BIGINT NOT NULL,

    -- updated_at is the unix timestamp of the latest row update.
    updated_at BIGINT NOT NULL,

    -- exit_policy_kind is the durable unroll exit policy selected when
    -- the job was admitted. Standard timeout jobs use
    -- 'standard_vtxo_timeout'; policy-specific jobs use the registered
    -- policy kind required to rebuild the same final spend after restart.
    exit_policy_kind TEXT NOT NULL DEFAULT 'standard_vtxo_timeout',

    -- exit_policy_ref optionally points at policy-specific durable state.
    -- It is NULL for standard VTXO timeout jobs.
    exit_policy_ref TEXT,

    PRIMARY KEY (target_outpoint_hash, target_outpoint_index)
);

CREATE INDEX IF NOT EXISTS idx_unilateral_exit_jobs_status_updated
    ON unilateral_exit_jobs(status, updated_at DESC);
