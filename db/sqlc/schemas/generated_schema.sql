CREATE TABLE chain_info (
    id INTEGER PRIMARY KEY,
    chain_name TEXT NOT NULL UNIQUE,
    genesis_hash BLOB NOT NULL
);

CREATE UNIQUE INDEX idx_forfeit_infos_outpoint
	ON round_forfeit_infos(outpoint_hash, outpoint_index);

CREATE INDEX idx_forfeit_infos_round
	ON round_forfeit_infos(round_id);

CREATE INDEX idx_indexer_receive_scripts_script
    ON indexer_receive_scripts(pk_script);

CREATE INDEX idx_indexer_vtxo_events_script_event
    ON indexer_vtxo_events(pk_script, event_id);

CREATE UNIQUE INDEX idx_mailbox_envelopes_dedup
    ON mailbox_envelopes(recipient, msg_id);

CREATE INDEX idx_mailbox_envelopes_recipient_seq
    ON mailbox_envelopes(recipient, event_seq);

CREATE INDEX idx_oor_recipient_events_session_db_id
    ON oor_recipient_events(session_db_id);

CREATE INDEX idx_oor_sessions_state_updated
    ON oor_sessions(state, updated_at);

CREATE INDEX idx_rounds_created_at
	ON rounds(created_at DESC);

CREATE INDEX idx_rounds_status
	ON rounds(status);

CREATE INDEX idx_rounds_txid
	ON rounds(commitment_txid);

CREATE INDEX idx_vtxo_tree_cosigners_key
	ON vtxo_tree_cosigners(cosigner_key, round_id, batch_output_index);

CREATE INDEX idx_vtxo_tree_nodes_depth
	ON vtxo_tree_nodes(round_id, batch_output_index, depth);

CREATE INDEX idx_vtxo_tree_nodes_leaves
	ON vtxo_tree_nodes(round_id, batch_output_index, is_leaf)
	WHERE is_leaf = 1;

CREATE INDEX idx_vtxo_tree_nodes_parent
	ON vtxo_tree_nodes(round_id, batch_output_index, parent_node_id);

CREATE INDEX idx_vtxos_locked
	ON vtxos(lock_owner_kind, lock_owner_id) WHERE lock_owner_id IS NOT NULL;

CREATE INDEX idx_vtxos_round
	ON vtxos(round_id);

CREATE INDEX idx_vtxos_status
	ON vtxos(status);

CREATE TABLE indexer_receive_scripts (
    -- principal_mailbox_id is the canonical mailbox id of the authenticated
    -- wallet principal (for example, "client:<id>").
    principal_mailbox_id TEXT NOT NULL,

    -- pk_script is the receive script bytes controlled by the principal.
    pk_script BLOB NOT NULL,

    -- expires_at_unix_s is an optional unix timestamp (seconds) after which
    -- this registration should be treated as inactive. Zero means no expiry.
    expires_at_unix_s BIGINT NOT NULL DEFAULT 0,

    -- label is optional client-provided metadata for debugging and UX.
    label TEXT NOT NULL DEFAULT '',

    -- updated_at is the unix nano timestamp of the latest registration write.
    updated_at BIGINT NOT NULL,

    PRIMARY KEY(principal_mailbox_id, pk_script)
);

CREATE TABLE indexer_vtxo_events (
    -- event_id is the global monotonic event cursor.
    event_id INTEGER PRIMARY KEY,

    -- pk_script is the script this event is scoped to.
    pk_script BLOB NOT NULL,

    -- event_type is one of:
    --   - created
    --   - status_changed
    --   - terminated
    event_type TEXT NOT NULL,

    -- outpoint_hash + outpoint_index identify the affected VTXO.
    outpoint_hash BLOB NOT NULL,
    outpoint_index INTEGER NOT NULL,

    -- status is the resulting VTXO status after the transition.
    status TEXT NOT NULL,

    -- created_at is the unix nano timestamp when the event row was written.
    created_at BIGINT NOT NULL
);

CREATE TABLE mailbox_ack_cursors (
    -- recipient is the mailbox identifier.
    recipient TEXT PRIMARY KEY,

    -- ack_cursor is the next expected event sequence number. All
    -- envelopes with event_seq < ack_cursor are considered acknowledged.
    ack_cursor BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE mailbox_envelopes (
    -- event_seq is the monotonically increasing sequence number assigned
    -- on append. It serves as both the primary key and the pull cursor
    -- target.
    event_seq INTEGER PRIMARY KEY,

    -- recipient identifies the target mailbox (e.g., "client-<id>" or
    -- "server-for-<id>").
    recipient TEXT NOT NULL,

    -- msg_id is the stable message identifier for deduplication.
    msg_id TEXT NOT NULL,

    -- envelope is the proto-serialized mailboxpb.Envelope bytes.
    envelope BLOB NOT NULL,

    -- created_at is the unix nano timestamp when the envelope was
    -- appended.
    created_at BIGINT NOT NULL
);

CREATE TABLE oor_checkpoints (
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

CREATE TABLE oor_recipient_events (
    -- recipient_pk_script is the destination script that owns this cursor
    -- sequence.
    recipient_pk_script BLOB NOT NULL,

    -- event_id is a per-recipient monotonic cursor assigned by the server.
    event_id BIGINT NOT NULL,

    -- session_db_id references the finalized OOR session integer PK.
    session_db_id INTEGER NOT NULL REFERENCES oor_sessions(id)
        ON DELETE CASCADE,

    -- output_index is the recipient output index in the Ark transaction.
    output_index INTEGER NOT NULL,

    -- value is the recipient output amount in satoshis.
    value BIGINT NOT NULL,

    -- created_at is the unix nano timestamp when the event row was written.
    created_at BIGINT NOT NULL,

    PRIMARY KEY(recipient_pk_script, event_id),

    -- Ensure idempotent inserts for the same recipient/session/output.
    UNIQUE (recipient_pk_script, session_db_id, output_index)
);

CREATE TABLE oor_sessions (
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

CREATE TABLE round_client_registrations (
	-- round_id links to the parent round.
	round_id BLOB NOT NULL,

	-- client_id is the unique client identifier (string).
	client_id BLOB NOT NULL,

	-- registration_data is the TLV-encoded ClientRegistration struct.
	-- Contains: BoardingInputs, LeaveOutputs, VTXODescriptors map, ForfeitInputs.
	registration_data BLOB NOT NULL,

	PRIMARY KEY (round_id, client_id),
	FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE
);

CREATE TABLE round_connector_descriptors (
	-- round_id links to the parent round.
	round_id BLOB NOT NULL,


	-- output_index is the connector output index in the commitment tx.
	output_index INTEGER NOT NULL,

	-- num_leaves is the number of connector leaves for this output.
	num_leaves INTEGER NOT NULL,

	-- forfeit_script is the penalty output script for forfeit transactions.
	forfeit_script BLOB NOT NULL,

	FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE
);

CREATE TABLE round_forfeit_infos (
	-- round_id is the round in which the VTXO was forfeited.
	round_id BLOB NOT NULL,

	-- outpoint_hash and outpoint_index identify the forfeited VTXO.
	outpoint_hash BLOB NOT NULL,
	outpoint_index INTEGER NOT NULL,

	-- forfeit_tx is the serialized wire.MsgTx (completed forfeit transaction).
	forfeit_tx BLOB NOT NULL,

	-- connector_output_index is the connector output index in the commitment tx.
	connector_output_index INTEGER NOT NULL,

	-- leaf_index is the leaf index within the connector tree.
	leaf_index INTEGER NOT NULL,

	PRIMARY KEY (round_id, outpoint_hash, outpoint_index),
	FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE,
	FOREIGN KEY (outpoint_hash, outpoint_index)
		REFERENCES vtxos(outpoint_hash, outpoint_index)
);

CREATE TABLE round_statuses (
	status TEXT PRIMARY KEY
);

CREATE TABLE round_vtxo_tree (
	-- round_id links to the parent round.
	round_id BLOB NOT NULL,

	-- batch_output_index is the commitment tx output index that roots this tree.
	batch_output_index INTEGER NOT NULL,

	PRIMARY KEY (round_id, batch_output_index),
	FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE
);

CREATE TABLE rounds (
	-- round_id is the unique identifier (16-byte UUID).
	round_id BLOB PRIMARY KEY NOT NULL,

	-- final_tx is the fully signed commitment transaction (wire.MsgTx serialized).
	-- NULL should never happen in practice since we only persist finalized rounds.
	final_tx BLOB NOT NULL,

	-- commitment_txid is the hex-encoded (byte-reversed) transaction ID.
	-- Stored as a string for easier dashboard/debugging use and efficient
	-- lookup during confirmation callbacks.
	commitment_txid TEXT NOT NULL UNIQUE,

	-- confirmation_height is the block height at which the commitment tx
	-- was confirmed. NULL until confirmed on-chain.
	confirmation_height INTEGER,

	-- confirmation_block_hash is the 32-byte hash of the block containing
	-- the commitment transaction. NULL until confirmed on-chain.
	confirmation_block_hash BLOB,

	-- status tracks round lifecycle (pending or confirmed).
	status TEXT NOT NULL DEFAULT 'pending',

	-- sweep_key is the 33-byte compressed operator public key used in the
	-- VTXO sweep timeout script. Required to reconstruct sweep scripts
	-- when recovering funds after CSV delay.
	sweep_key BLOB NOT NULL,

	-- csv_delay is the relative timelock (in blocks) for the VTXO sweep
	-- timeout path. Required to reconstruct sweep scripts and spend VTXOs
	-- unilaterally after the delay.
	csv_delay INTEGER NOT NULL,

	-- created_at is the unix epoch timestamp when this round was created.
	created_at BIGINT NOT NULL,

	-- updated_at is the unix epoch timestamp of the last update.
	updated_at BIGINT NOT NULL,

	FOREIGN KEY (status) REFERENCES round_statuses(status)
);

CREATE TABLE vtxo_statuses (
	status TEXT PRIMARY KEY
);

CREATE TABLE vtxo_tree_cosigners (
	-- Links to parent node.
	round_id BLOB NOT NULL,
	batch_output_index INTEGER NOT NULL,
	node_id TEXT NOT NULL,

	-- Cosigner key (compressed 33-byte public key).
	cosigner_key BLOB NOT NULL,

	-- Position in the cosigner list (for ordering).
	key_index INTEGER NOT NULL,

	PRIMARY KEY (round_id, batch_output_index, node_id, key_index),
	FOREIGN KEY (round_id, batch_output_index, node_id)
		REFERENCES vtxo_tree_nodes(round_id, batch_output_index, node_id)
		ON DELETE CASCADE
);

CREATE TABLE vtxo_tree_node_outputs (
	-- Links to parent node.
	round_id BLOB NOT NULL,
	batch_output_index INTEGER NOT NULL,
	node_id TEXT NOT NULL,

	-- Output index in the transaction.
	output_index INTEGER NOT NULL,

	-- Output details.
	value BIGINT NOT NULL,
	pk_script BLOB NOT NULL,

	PRIMARY KEY (round_id, batch_output_index, node_id, output_index),
	FOREIGN KEY (round_id, batch_output_index, node_id)
		REFERENCES vtxo_tree_nodes(round_id, batch_output_index, node_id)
		ON DELETE CASCADE
);

CREATE TABLE vtxo_tree_nodes (
	-- Composite key linking to parent tree.
	round_id BLOB NOT NULL,
	batch_output_index INTEGER NOT NULL,

	-- Node identifier within the tree.
	-- Uses path notation: "0" for root, "0.1" for first child, "0.1.2" for
	-- second child of first child, etc. This makes it easy to query
	-- ancestors and descendants.
	node_id TEXT NOT NULL,

	-- Tree structure fields.
	parent_node_id TEXT,
	parent_output_index INTEGER,
	depth INTEGER NOT NULL,
	is_leaf INTEGER NOT NULL,

	-- Transaction input that this node spends.
	input_hash BLOB NOT NULL,
	input_index INTEGER NOT NULL,

	-- Node attributes.
	amount BIGINT NOT NULL,

	-- Optional fields populated after signing.
	signature BLOB,
	final_key BLOB,

	PRIMARY KEY (round_id, batch_output_index, node_id),
	FOREIGN KEY (round_id, batch_output_index)
		REFERENCES round_vtxo_tree(round_id, batch_output_index)
		ON DELETE CASCADE,
	FOREIGN KEY (round_id, batch_output_index, parent_node_id)
		REFERENCES vtxo_tree_nodes(round_id, batch_output_index, node_id)
		ON DELETE CASCADE
);

CREATE TABLE vtxos (
	-- outpoint_hash and outpoint_index form the VTXO outpoint (primary key).
	outpoint_hash BLOB NOT NULL,
	outpoint_index INTEGER NOT NULL,

	-- round_id links to the round that created this VTXO.
	-- NULL for VTXOs created by virtual transactions (future feature).
	-- NOT NULL for VTXOs created directly in rounds (current implementation).
	round_id BLOB,

	-- batch_output_index is the commitment tx output index that roots the
	-- VTXO tree containing this VTXO.
	-- NULL for VTXOs created by virtual transactions (future feature).
	-- NOT NULL for VTXOs created directly in rounds (current implementation).
	batch_output_index INTEGER,

	-- VTXO descriptor fields (from tree.VTXODescriptor).
	-- amount is the value of this VTXO in satoshis.
	amount BIGINT NOT NULL,

	-- pk_script is the P2TR script for the VTXO output.
	pk_script BLOB NOT NULL,

	-- cosigner_key is the 33-byte compressed public key of the VTXO owner.
	--
	-- This key is always required for spend path reconstruction.
	cosigner_key BLOB NOT NULL,

	-- status tracks VTXO lifecycle (pending, live, in_flight, forfeited, spent).
	status TEXT NOT NULL DEFAULT 'pending',

	-- lock_owner_kind identifies who owns the in-flight lock.
	-- NULL when unlocked.
	lock_owner_kind TEXT,

	-- lock_owner_id identifies the lock owner instance within the kind.
	-- NULL when unlocked.
	lock_owner_id BLOB,

	PRIMARY KEY (outpoint_hash, outpoint_index),
	FOREIGN KEY (round_id) REFERENCES rounds(round_id),
	FOREIGN KEY (status) REFERENCES vtxo_statuses(status),
	CHECK (lock_owner_kind IS NULL OR lock_owner_kind IN ('round', 'oor')),
	CHECK ((lock_owner_kind IS NULL) = (lock_owner_id IS NULL)),
	CHECK ((status = 'in_flight') = (lock_owner_kind IS NOT NULL))
);

