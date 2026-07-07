-- Out-of-round (OOR) transfer state: the durable artifact store for
-- finalized packages, receiver-side polling state, and the OOR
-- session registry that owns per-session durable actors.

-- OOR artifact store tables support durable local package retrieval for
-- both incoming and outgoing OOR sessions.

-- oor_package_directions is the enum table for package direction codes.
CREATE TABLE IF NOT EXISTS oor_package_directions (
    -- direction is the persisted package direction code.
    direction INTEGER PRIMARY KEY NOT NULL,

    -- name is the stable string representation of the direction code.
    name TEXT NOT NULL UNIQUE
);

INSERT INTO oor_package_directions (direction, name) VALUES
    (0, 'incoming'),
    (1, 'outgoing')
ON CONFLICT DO NOTHING;

-- oor_vtxo_binding_link_kinds is the enum table for binding relation kinds.
CREATE TABLE IF NOT EXISTS oor_vtxo_binding_link_kinds (
    -- link_kind is the persisted relation code.
    link_kind INTEGER PRIMARY KEY NOT NULL,

    -- name is the stable string representation of the relation code.
    name TEXT NOT NULL UNIQUE
);

INSERT INTO oor_vtxo_binding_link_kinds (link_kind, name) VALUES
    (0, 'created_output'),
    (1, 'consumed_input')
ON CONFLICT DO NOTHING;

-- owned_receive_script_sources is the enum table for script discovery source.
CREATE TABLE IF NOT EXISTS owned_receive_script_sources (
    -- source is the persisted source code.
    source INTEGER PRIMARY KEY NOT NULL,

    -- name is the stable string representation of the source code.
    name TEXT NOT NULL UNIQUE
);

INSERT INTO owned_receive_script_sources (source, name) VALUES
    (0, 'wallet'),
    (1, 'rpc'),
    (2, 'sync')
ON CONFLICT DO NOTHING;

-- oor_packages stores one finalized OOR package artifact set per session.
CREATE TABLE IF NOT EXISTS oor_packages (
    -- session_id is the stable OOR session identifier (Ark txid bytes).
    session_id BLOB PRIMARY KEY NOT NULL,

    -- direction encodes local package direction:
    --   0 = incoming (received by this client)
    --   1 = outgoing (sent by this client)
    direction INTEGER NOT NULL,

    -- ark_psbt is the canonical Ark transaction package.
    ark_psbt BLOB NOT NULL,

    -- created_at is the unix timestamp when the row was first written.
    created_at BIGINT NOT NULL,

    -- updated_at is the unix timestamp of the last row update.
    updated_at BIGINT NOT NULL,

    -- Direction enum foreign key.
    FOREIGN KEY (direction) REFERENCES oor_package_directions(direction)
);

-- oor_package_checkpoints stores the ordered finalized checkpoint package
-- for each OOR session.
CREATE TABLE IF NOT EXISTS oor_package_checkpoints (
    -- session_id references the owning OOR package row.
    session_id BLOB NOT NULL,

    -- checkpoint_index is the zero-based order inside the package.
    checkpoint_index INTEGER NOT NULL CHECK (checkpoint_index >= 0),

    -- checkpoint_psbt stores one serialized finalized checkpoint PSBT.
    checkpoint_psbt BLOB NOT NULL,

    -- created_at is the unix timestamp when this index row was inserted.
    created_at BIGINT NOT NULL,

    -- Primary key keeps one checkpoint row per package index.
    PRIMARY KEY (session_id, checkpoint_index),

    -- Session foreign key keeps checkpoint rows tied to package lifecycle.
    FOREIGN KEY (session_id) REFERENCES oor_packages(session_id)
        ON DELETE CASCADE
);

-- oor_vtxo_bindings maps local outpoints to stored OOR package sessions.
CREATE TABLE IF NOT EXISTS oor_vtxo_bindings (
    -- outpoint identifies the local VTXO outpoint linked to this package.
    outpoint_hash BLOB NOT NULL,

    -- outpoint_index is the output index of the local outpoint.
    outpoint_index INTEGER NOT NULL CHECK (outpoint_index >= 0),

    -- session_id references the OOR package linked to this outpoint.
    session_id BLOB NOT NULL,

    -- output_index identifies the package output index (incoming) or
    -- enumerated input index (outgoing consumed input).
    output_index INTEGER NOT NULL CHECK (output_index >= 0),

    -- link_kind encodes outpoint relation to package:
    --   0 = created_output (outpoint created by Ark package)
    --   1 = consumed_input (outpoint consumed by outgoing package)
    link_kind INTEGER NOT NULL,

    -- recipient script and amount are intentionally not duplicated here.
    -- They are derived from the referenced vtxos row via outpoint joins.

    -- created_at is the unix timestamp when the binding was created.
    created_at BIGINT NOT NULL,

    -- updated_at is the unix timestamp of the last binding update.
    updated_at BIGINT NOT NULL,

    -- Primary key allows both created-output and consumed-input bindings
    -- to coexist for the same local outpoint.
    PRIMARY KEY (outpoint_hash, outpoint_index, link_kind),

    -- Unique key prevents duplicate relation rows for one session member.
    UNIQUE (session_id, output_index, link_kind),

    -- Session foreign key keeps bindings tied to package lifecycle.
    FOREIGN KEY (session_id) REFERENCES oor_packages(session_id)
        ON DELETE CASCADE,

    -- Link-kind enum foreign key.
    FOREIGN KEY (link_kind) REFERENCES oor_vtxo_binding_link_kinds(link_kind),

    -- Outpoint foreign key enforces that bindings only reference local
    -- VTXOs known to the round/vtxo persistence tables.
    FOREIGN KEY (outpoint_hash, outpoint_index) REFERENCES vtxos(
        outpoint_hash, outpoint_index
    )
);

-- oor_recipient_cursors stores the last processed recipient event for each
-- tracked recipient script.
-- These cursors are used by the receiver-side polling flow against server
-- recipient events, where each event can be expanded back to finalized Ark
-- and checkpoint package artifacts.
CREATE TABLE IF NOT EXISTS oor_recipient_cursors (
    -- recipient_pk_script is the tracked recipient script key.
    recipient_pk_script BLOB PRIMARY KEY NOT NULL,

    -- last_event_id is the last successfully processed event ID.
    last_event_id BIGINT NOT NULL,

    -- updated_at is the unix timestamp of the last cursor update.
    updated_at BIGINT NOT NULL,

    -- last_session_id is the last processed session for debugging.
    last_session_id BLOB
);

-- owned_receive_scripts stores locally owned receive script metadata used
-- to drive recipient polling and package attribution.
-- This table is local receiver state used to decide which recipient event
-- streams to poll and how to materialize matching outputs into wallet VTXOs.
CREATE TABLE IF NOT EXISTS owned_receive_scripts (
    -- pk_script is the owned receive script primary key.
    pk_script BLOB PRIMARY KEY NOT NULL,

    -- client_key_id references the internal_keys registry row for the client
    -- wallet key used in the checkpoint taptree. The registry row carries the
    -- compressed pubkey plus the lnd KeyLocator. Declared nullable only for
    -- uniformity with the genuinely-optional internal_keys FKs (vtxos,
    -- round_vtxo_requests); in practice every owned receive script has a
    -- client key, so the write path always registers it first and the read
    -- path treats a NULL as an error.
    client_key_id BIGINT REFERENCES internal_keys(id),

    -- operator_pubkey is the operator key used in the checkpoint taptree.
    operator_pubkey BLOB NOT NULL,

    -- exit_delay is the CSV delay used in the timeout branch.
    exit_delay BIGINT NOT NULL,

    -- source labels how this script was discovered/registered:
    --   0 = wallet
    --   1 = rpc
    --   2 = sync
    source INTEGER NOT NULL CHECK (source IN (0, 1, 2)),

    -- created_at is the unix timestamp when this script was registered.
    created_at BIGINT NOT NULL,

    -- last_used_at is an optional unix timestamp of latest usage.
    last_used_at BIGINT,

    -- Source enum foreign key.
    FOREIGN KEY (source) REFERENCES owned_receive_script_sources(source)
);

-- Index speeds list/filter calls for incoming/outgoing package queries.
CREATE INDEX IF NOT EXISTS idx_oor_packages_direction_updated
    ON oor_packages(direction, updated_at DESC);

-- Index speeds loading ordered checkpoint sets by session.
CREATE INDEX IF NOT EXISTS idx_oor_package_checkpoints_session
    ON oor_package_checkpoints(session_id, checkpoint_index ASC);

-- Index speeds loading all bindings for one package session.
CREATE INDEX IF NOT EXISTS idx_oor_vtxo_bindings_session
    ON oor_vtxo_bindings(session_id);

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

    -- flow_version is the permanent OOR flow version this session was
    -- conducted under (the protocol choreography, distinct from
    -- snapshot_version which only versions the resume blob's encoding). It is
    -- stamped write-once at creation and validated on load. The versions are
    -- zero-indexed, so the DEFAULT 0 is V1 (the Go zero value); V2 == 1, and
    -- so on.
    flow_version INTEGER NOT NULL DEFAULT 0,

    -- created_at is the unix timestamp when the row was first written.
    created_at BIGINT NOT NULL,

    -- updated_at is the unix timestamp of the latest row update.
    updated_at BIGINT NOT NULL,

    PRIMARY KEY (session_id)
);

-- The (status, created_at) index serves the boot-time non-terminal restore
-- scan and any future bounded-retention sweep of terminal rows. Terminal rows
-- (completed and failed, all directions) are retained for status RPCs and
-- diagnostics; they are not pruned at reap time.
CREATE INDEX IF NOT EXISTS idx_oor_session_registry_status_created
    ON oor_session_registry(status, created_at ASC);

-- At most one live-or-completed session may carry a given idempotency key:
-- the partial UNIQUE index enforces the dedup invariant in the schema rather
-- than in Go. Failed rows (status 2) drop out of the index so a keyed retry
-- after a failure can admit a fresh session under the same key.
CREATE UNIQUE INDEX IF NOT EXISTS idx_oor_session_registry_idempotency_key
    ON oor_session_registry(idempotency_key)
    WHERE idempotency_key IS NOT NULL AND status != 2;
