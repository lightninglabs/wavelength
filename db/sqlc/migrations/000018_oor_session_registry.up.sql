-- oor_session_registry stores the full durable state for one OOR session per
-- session id. The OOR registry actor owns these rows: it spawns one durable
-- per-session actor per non-terminal record and restores them on boot. This
-- table is the single source of truth for OOR session state -- the per-session
-- actor reads and writes it directly inside its Read/Stage/Commit phases and
-- does NOT use the generic actor-delivery fsm_checkpoints blob. The
-- control-plane fields (direction, phase, idempotency_key, status) are
-- first-class queryable columns; the irreducible opaque resume material
-- (operator co-signed checkpoint PSBTs, transfer-input signing snapshots) rides
-- in snapshot_data, which nothing queries by and only needs to round-trip.
CREATE TABLE IF NOT EXISTS oor_session_registry (
    -- session_id is the 32-byte OOR session identifier (Ark txid hash in v0).
    session_id BLOB NOT NULL,

    -- actor_id is the durable actor mailbox id for this session's per-session
    -- actor. Deterministically derived from session_id.
    actor_id TEXT NOT NULL,

    -- direction records whether this session is locally sent or received:
    --   1 = outgoing
    --   2 = incoming
    direction INTEGER NOT NULL,

    -- phase is the latest control-plane phase string (the OutgoingPhase /
    -- IncomingPhase value), kept as a queryable column for diagnostics and
    -- restore filtering.
    phase TEXT NOT NULL,

    -- idempotency_key is the outgoing-session idempotency key used to dedup a
    -- repeated StartTransferRequest. NULL for incoming sessions and for
    -- outgoing sessions started without an explicit key.
    idempotency_key TEXT,

    -- status is the coordinator-facing session status:
    --   0 = pending (in flight)
    --   1 = completed
    --   2 = failed
    status INTEGER NOT NULL,

    -- last_error stores the latest terminal failure reason.
    last_error TEXT,

    -- snapshot_data is the TLV-encoded per-session resume snapshot (the
    -- OutgoingSnapshot / IncomingSnapshot). It carries the signing material the
    -- session must replay with byte-for-byte after a restart (notably the
    -- operator co-signed checkpoint PSBTs past the point of no return). NULL
    -- only in the brief admission window before the session's first staged
    -- write.
    snapshot_data BLOB,

    -- snapshot_version is the encoding version of snapshot_data.
    snapshot_version INTEGER NOT NULL DEFAULT 0,

    -- created_at is the unix timestamp when the row was first written.
    created_at BIGINT NOT NULL,

    -- updated_at is the unix timestamp of the latest row update.
    updated_at BIGINT NOT NULL,

    PRIMARY KEY (session_id)
);

CREATE INDEX IF NOT EXISTS idx_oor_session_registry_status_created
    ON oor_session_registry(status, created_at ASC);

-- At most one live-or-completed session may carry a given idempotency key:
-- the partial UNIQUE index enforces the dedup invariant in the schema rather
-- than in Go. Failed rows (status 2) drop out of the index so a keyed retry
-- after a failure can admit a fresh session under the same key.
CREATE UNIQUE INDEX IF NOT EXISTS idx_oor_session_registry_idempotency_key
    ON oor_session_registry(idempotency_key)
    WHERE idempotency_key IS NOT NULL AND status != 2;
