-- OOR artifact store tables support durable local package retrieval for
-- both incoming and outgoing OOR sessions.

-- oor_packages stores one finalized OOR package artifact set per session.
CREATE TABLE IF NOT EXISTS oor_packages (
    -- session_id is the stable OOR session identifier (Ark txid bytes).
    session_id BLOB PRIMARY KEY NOT NULL,

    -- direction indicates if this package was received or sent by this client.
    direction TEXT NOT NULL CHECK (
        direction IN ('incoming', 'outgoing')
    ),

    -- ark_psbt is the canonical Ark transaction package.
    ark_psbt BLOB NOT NULL,

    -- created_at is the unix timestamp when the row was first written.
    created_at BIGINT NOT NULL,

    -- updated_at is the unix timestamp of the last row update.
    updated_at BIGINT NOT NULL
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

    -- link_kind identifies what this outpoint means relative to the package.
    link_kind TEXT NOT NULL CHECK (
        link_kind IN ('created_output', 'consumed_input')
    ),

    -- recipient_pk_script is set for created-output bindings.
    recipient_pk_script BLOB,

    -- value_sat is set for created-output bindings when available.
    value_sat BIGINT,

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
        ON DELETE CASCADE
);

-- oor_recipient_cursors stores the last processed recipient event for each
-- tracked recipient script.
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
CREATE TABLE IF NOT EXISTS owned_receive_scripts (
    -- pk_script is the owned receive script primary key.
    pk_script BLOB PRIMARY KEY NOT NULL,

    -- client_key_family is the wallet key family for this script.
    client_key_family BIGINT NOT NULL,

    -- client_key_index is the wallet key index for this script.
    client_key_index BIGINT NOT NULL,

    -- client_pubkey is the client key used in the checkpoint taptree.
    client_pubkey BLOB NOT NULL,

    -- operator_pubkey is the operator key used in the checkpoint taptree.
    operator_pubkey BLOB NOT NULL,

    -- exit_delay is the CSV delay used in the timeout branch.
    exit_delay BIGINT NOT NULL,

    -- source labels how this script was discovered/registered.
    source TEXT NOT NULL,

    -- created_at is the unix timestamp when this script was registered.
    created_at BIGINT NOT NULL,

    -- last_used_at is an optional unix timestamp of latest usage.
    last_used_at BIGINT
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

-- Index speeds recipient-script based binding lookups.
CREATE INDEX IF NOT EXISTS idx_oor_vtxo_bindings_script
    ON oor_vtxo_bindings(recipient_pk_script);
