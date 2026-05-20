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

    -- exit_policy_kind identifies the policy used for the final exit spend.
    -- The resolver validates this extension point so future policy kinds do
    -- not require rebuilding this table on SQLite.
    exit_policy_kind TEXT NOT NULL,

    -- exit_policy_ref optionally points at policy-specific durable state.
    exit_policy_ref TEXT,

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

CREATE TABLE IF NOT EXISTS unroll_tx_progress (
    target_outpoint_hash BLOB NOT NULL,
    target_outpoint_index INTEGER NOT NULL CHECK (
        target_outpoint_index >= 0
    ),
    txid BLOB NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('proof', 'deferred_checkpoint', 'sweep')),
    status TEXT NOT NULL CHECK (status IN (
        'ready',
        'in_flight',
        'confirmed',
        'failed'
    )),
    tx_bytes BLOB,
    confirm_height INTEGER,
    last_error TEXT,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,

    PRIMARY KEY (
        target_outpoint_hash, target_outpoint_index, txid, role
    ),
    FOREIGN KEY (target_outpoint_hash, target_outpoint_index)
        REFERENCES unroll_jobs(target_outpoint_hash, target_outpoint_index)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_unroll_tx_progress_status
    ON unroll_tx_progress(status, updated_at DESC);

CREATE TABLE IF NOT EXISTS unroll_watches (
    target_outpoint_hash BLOB NOT NULL,
    target_outpoint_index INTEGER NOT NULL CHECK (
        target_outpoint_index >= 0
    ),
    watch_id TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN (
        'block_epoch',
        'target_spend',
        'proof_tx',
        'deferred_checkpoint',
        'sweep'
    )),
    txid BLOB,
    spend_outpoint_hash BLOB,
    spend_outpoint_index INTEGER,
    status TEXT NOT NULL CHECK (status IN (
        'registered',
        'confirmed',
        'spent',
        'cancelled',
        'failed'
    )),
    height_hint INTEGER,
    confirmation_height INTEGER,
    last_error TEXT,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,

    PRIMARY KEY (target_outpoint_hash, target_outpoint_index, watch_id),
    FOREIGN KEY (target_outpoint_hash, target_outpoint_index)
        REFERENCES unroll_jobs(target_outpoint_hash, target_outpoint_index)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_unroll_watches_status
    ON unroll_watches(status, role, updated_at DESC);

CREATE TABLE IF NOT EXISTS unroll_effects (
    id TEXT PRIMARY KEY,
    target_outpoint_hash BLOB NOT NULL,
    target_outpoint_index INTEGER NOT NULL CHECK (
        target_outpoint_index >= 0
    ),
    effect_type TEXT NOT NULL CHECK (effect_type IN (
        'subscribe_blocks',
        'watch_target_spend',
        'ensure_tx_confirmed',
        'watch_deferred_checkpoint',
        'build_sweep',
        'ensure_sweep_confirmed',
        'notify_registry'
    )),
    txid BLOB,
    status TEXT NOT NULL CHECK (status IN (
        'pending',
        'claimed',
        'done',
        'dead'
    )),
    idempotency_key TEXT NOT NULL UNIQUE,
    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 10,
    next_attempt_at BIGINT NOT NULL,
    claim_owner TEXT,
    claim_token TEXT,
    claim_until BIGINT,
    last_error TEXT,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    done_at BIGINT,

    FOREIGN KEY (target_outpoint_hash, target_outpoint_index)
        REFERENCES unroll_jobs(target_outpoint_hash, target_outpoint_index)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_unroll_effects_due
    ON unroll_effects(status, next_attempt_at, created_at);
