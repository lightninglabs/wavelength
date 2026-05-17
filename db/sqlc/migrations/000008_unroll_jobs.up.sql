-- unroll_jobs stores the restart-safe FSM row for one VTXO unroll target.
CREATE TABLE IF NOT EXISTS unroll_jobs (
    -- target_outpoint_hash identifies the target transaction.
    target_outpoint_hash BLOB NOT NULL,

    -- target_outpoint_index identifies the target output index.
    target_outpoint_index INTEGER NOT NULL CHECK (
        target_outpoint_index >= 0
    ),

    -- state is the visible unroll FSM phase.
    state TEXT NOT NULL CHECK (state IN (
        'pending',
        'materializing',
        'csv_pending',
        'sweep_broadcast',
        'sweep_confirmation',
        'completed',
        'failed'
    )),

    -- trigger identifies what started the job.
    trigger TEXT NOT NULL CHECK (trigger IN (
        'manual',
        'critical_expiry',
        'restart',
        'fraud_spend'
    )),

    -- best_height is the latest chain height observed by this job.
    best_height INTEGER NOT NULL,

    -- target_confirm_height records the target confirmation height once known.
    target_confirm_height INTEGER,

    -- planner_state is the encoded unroll planner graph cursor.
    planner_state BLOB NOT NULL,

    -- deferred_checkpoints records fraud-triggered checkpoint deferrals.
    deferred_checkpoints BLOB,

    -- sweep_tx stores the exact final sweep transaction bytes after build.
    sweep_tx BLOB,

    -- sweep_txid is the 32-byte txid of the final sweep transaction once known.
    sweep_txid BLOB,

    -- sweep_confirm_height records the sweep confirmation height when known.
    sweep_confirm_height INTEGER,

    -- sweep_attempts counts sweep build/broadcast attempts.
    sweep_attempts INTEGER NOT NULL DEFAULT 0,

    -- fail_reason stores the terminal failure when present.
    fail_reason TEXT,

    -- created_at is the unix timestamp when the row was first written.
    created_at BIGINT NOT NULL,

    -- updated_at is the unix timestamp of the latest row update.
    updated_at BIGINT NOT NULL,

    PRIMARY KEY (target_outpoint_hash, target_outpoint_index)
);

CREATE INDEX IF NOT EXISTS idx_unroll_jobs_state_updated
    ON unroll_jobs(state, updated_at DESC);
