-- pending_intents generalizes the restart-safe intent outbox that
-- pending_board_requests pioneered for the Board RPC. A row records a
-- user-issued intent (Board, SendOnChain, ...) that has been accepted by the
-- daemon but not yet durably adopted by a round, so a daemon restart inside
-- that window can replay the intent instead of silently dropping it.
--
-- Each intent is anchored to the set of outpoints the round consumes when it
-- adopts the intent: confirmed boarding outpoints for Board, reserved forfeit
-- VTXO outpoints for SendOnChain. Anchor rows are cleared by outpoint inside
-- the same SQL transaction as the round-state checkpoint that adopts the
-- intent (db.RoundPersistenceStore.CommitState), so replay-after-adoption is
-- structurally impossible: adoption consumes the anchor and deletes the row
-- atomically. An intent whose anchors have all been cleared is stale and is
-- swept by the same transaction.
--
-- Rather than an opaque payload blob, the kind-specific replay parameters
-- live in per-kind detail tables as first-class typed columns. A thin
-- pending_intents header carries only the identity, the kind discriminator,
-- and the request time, so the shared anchor table can foreign-key one place
-- and the round-state checkpoint clears anchors without knowing the kind.
--
-- This migration replaces pending_board_requests wholesale. The project is
-- in alpha, so in-flight rows (only ever populated inside the narrow crash
-- window between Board admission and round seal) are dropped rather than
-- migrated; a dropped row degrades to the user re-issuing the Board call.
DROP TABLE IF EXISTS pending_board_requests;

-- pending_intent_kinds is the enum table of valid intent kinds. The header's
-- kind column foreign-keys here so an unknown discriminator is rejected at
-- the DB layer rather than silently persisted.
CREATE TABLE IF NOT EXISTS pending_intent_kinds (
    kind TEXT PRIMARY KEY
);

INSERT INTO pending_intent_kinds (kind) VALUES
    ('board'),
    ('send_onchain');

-- pending_intents is the kind-agnostic header: one row per accepted intent.
-- The typed replay parameters live in the per-kind detail table selected by
-- kind. No payload blob: every field is a first-class column in its detail
-- table.
CREATE TABLE IF NOT EXISTS pending_intents (
    -- intent_id is a 32-byte identifier derived in Go by hashing the intent
    -- kind, the sorted anchor outpoints, and the payload's canonical field
    -- encoding. Re-persisting the same logical intent upserts; a tampered
    -- detail row no longer hashes to its id and is dropped on replay.
    intent_id BLOB PRIMARY KEY CHECK (length(intent_id) = 32),

    -- kind discriminates which detail table holds this intent's parameters.
    kind TEXT NOT NULL REFERENCES pending_intent_kinds(kind),

    -- requested_at_unix is when the user issued the intent. Replay
    -- diagnostics surface it; newer intents win when reconciling.
    requested_at_unix BIGINT NOT NULL CHECK (requested_at_unix > 0)
);

CREATE INDEX IF NOT EXISTS idx_pending_intents_kind
ON pending_intents (kind);

-- pending_board_intents holds the Board replay parameters.
CREATE TABLE IF NOT EXISTS pending_board_intents (
    intent_id BLOB PRIMARY KEY
        REFERENCES pending_intents(intent_id),

    -- target_vtxo_count mirrors BoardRequest.TargetVTXOCount: zero collapses
    -- the confirmed boarding balance into one VTXO, non-zero fans it out.
    target_vtxo_count INTEGER NOT NULL DEFAULT 0
        CHECK (target_vtxo_count >= 0)
);

-- pending_send_intents holds the SendOnChain replay parameters. Every field
-- the replay rebuild needs beyond the anchor (forfeit) outpoints is a typed
-- column here.
CREATE TABLE IF NOT EXISTS pending_send_intents (
    intent_id BLOB PRIMARY KEY
        REFERENCES pending_intents(intent_id),

    -- dest_pkscript is the on-chain destination script of the leave output.
    dest_pkscript BLOB NOT NULL CHECK (length(dest_pkscript) > 0),

    -- target_amount_sat is the exact amount to land at the destination in
    -- bounded mode; zero in sweep-all mode.
    target_amount_sat BIGINT NOT NULL CHECK (target_amount_sat >= 0),

    -- sweep_all marks the sweep-all mode where the single leave output
    -- absorbs the seal-time residual instead of a fixed amount plus change.
    sweep_all INTEGER NOT NULL CHECK (sweep_all IN (0, 1)),

    -- operator_key is the operator pubkey for the change-VTXO policy
    -- template. NULL in sweep-all mode (no change VTXO is built).
    operator_key BLOB
        CHECK (operator_key IS NULL OR length(operator_key) = 33),

    -- vtxo_exit_delay is the CSV delay of the change VTXO's exit path.
    -- Unused (zero) in sweep-all mode.
    vtxo_exit_delay INTEGER NOT NULL DEFAULT 0
        CHECK (vtxo_exit_delay >= 0),

    -- dust_limit_sat is the change-VTXO dust floor used for the defensive
    -- re-validation on replay. Unused (zero) in sweep-all mode.
    dust_limit_sat BIGINT NOT NULL DEFAULT 0
        CHECK (dust_limit_sat >= 0)
);

CREATE TABLE IF NOT EXISTS pending_intent_anchors (
    -- The anchored outpoint. For kind='board' this is a confirmed boarding
    -- outpoint; for kind='send_onchain' a reserved forfeit VTXO outpoint.
    outpoint_hash BLOB NOT NULL CHECK (length(outpoint_hash) = 32),
    outpoint_index INTEGER NOT NULL CHECK (outpoint_index >= 0),

    -- The owning intent header. Deleting an intent requires deleting its
    -- anchors and detail row in the same transaction (the store does this
    -- explicitly rather than relying on cascade semantics differing across
    -- backends).
    intent_id BLOB NOT NULL REFERENCES pending_intents(intent_id),

    -- One anchor row per outpoint across ALL intents: a newer intent that
    -- claims an already-anchored outpoint rebinds it (upsert), preserving
    -- the pending_board_requests semantics where a fresh Board call took
    -- over the rows of a prior one.
    PRIMARY KEY (outpoint_hash, outpoint_index)
);

CREATE INDEX IF NOT EXISTS idx_pending_intent_anchors_intent_id
ON pending_intent_anchors (intent_id);
