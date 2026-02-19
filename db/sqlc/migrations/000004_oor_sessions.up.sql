-- OOR session persistence schema.
--
-- This introduces durable storage for OOR transfer sessions. The coordinator
-- persists session state, Ark PSBT, and per-input checkpoint rows used for
-- crash-resume, idempotent replay, and recipient notification.
--
-- NOTE: This schema is written to be compatible with both sqlite and postgres
-- backends.

CREATE TABLE IF NOT EXISTS oor_sessions (
    -- id is the auto-assigned integer primary key used as a compact FK
    -- target by child tables.
    id INTEGER PRIMARY KEY,

    -- session_id is the deterministic Ark txid (32 bytes) and the
    -- external natural key used by callers.
    session_id BLOB NOT NULL UNIQUE,

    -- state tracks the session lifecycle stage.
    state TEXT NOT NULL CHECK (state IN (
        'cosigned', 'awaiting_notify', 'finalized', 'failed'
    )),

    -- ark_psbt stores the canonical Ark package PSBT bytes, written at
    -- co-sign time and never overwritten.
    ark_psbt BLOB NOT NULL,

    -- created_at is the unix nano timestamp when this session row was
    -- first created.
    created_at BIGINT NOT NULL,

    -- updated_at is the unix nano timestamp of the most recent session
    -- update.
    updated_at BIGINT NOT NULL,

    -- expires_at is the unix nano timestamp used to garbage-collect stale
    -- co-signed sessions.
    expires_at BIGINT NOT NULL,

    -- finalized_at is the unix nano timestamp when finalize succeeded.
    -- It remains NULL until ApplyFinalize succeeds.
    finalized_at BIGINT
);

-- Composite index for ListActiveOORSessions and state-scoped scans.
CREATE INDEX IF NOT EXISTS idx_oor_sessions_state_updated
    ON oor_sessions(state, updated_at);

-- oor_checkpoints stores per-input checkpoint PSBTs for a session.
--
-- Each row represents one checkpoint input: the VTXO outpoint being spent
-- and the corresponding checkpoint PSBT bytes. At co-sign time the PSBT
-- contains operator signature material; at finalize time it is overwritten
-- with the fully-signed client PSBT.
--
-- The UNIQUE constraint on (input_txid, input_vout) prevents two sessions
-- from claiming the same VTXO input.
CREATE TABLE IF NOT EXISTS oor_checkpoints (
    -- session_db_id references the parent OOR session integer PK.
    session_db_id INTEGER NOT NULL
        REFERENCES oor_sessions(id) ON DELETE CASCADE,

    -- checkpoint_index preserves deterministic package ordering.
    checkpoint_index INTEGER NOT NULL,

    -- input_txid is the 32-byte outpoint transaction hash for the claimed
    -- input.
    input_txid BLOB NOT NULL,

    -- input_vout is the outpoint index for the claimed input.
    input_vout INTEGER NOT NULL,

    -- checkpoint_psbt is the serialized checkpoint PSBT bytes (co-signed
    -- initially, finalized after ApplyFinalize).
    checkpoint_psbt BLOB NOT NULL,

    PRIMARY KEY(session_db_id, checkpoint_index),

    -- Ensure no two sessions can claim the same input.
    UNIQUE(input_txid, input_vout)
);
