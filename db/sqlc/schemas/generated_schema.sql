CREATE TABLE account_types (
    account_type TEXT PRIMARY KEY
);

CREATE TABLE accounts (
    -- account_id is the short mnemonic for the account (e.g.
    -- 'treasury_wallet').
    account_id TEXT PRIMARY KEY,

    -- account_name is the human-readable label.
    account_name TEXT NOT NULL,

    -- account_type classifies the account for reporting.
    account_type TEXT NOT NULL
        REFERENCES account_types(account_type)
);

CREATE TABLE chain_info (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    chain_name TEXT NOT NULL UNIQUE,
    genesis_hash BLOB NOT NULL
);

CREATE TABLE fee_schedule_history (
    -- id is the monotonically increasing primary key.
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    -- annual_rate is the cost-of-capital rate at time of change.
    annual_rate DOUBLE PRECISION NOT NULL,

    -- base_margin_sat is the fixed operator margin.
    base_margin_sat BIGINT NOT NULL,

    -- util_threshold_bps is the congestion threshold.
    util_threshold_bps INTEGER NOT NULL,

    -- util_spread_delta0_bps is the base congestion spread.
    util_spread_delta0_bps INTEGER NOT NULL,

    -- util_spread_delta1_bps is the linear congestion spread.
    util_spread_delta1_bps INTEGER NOT NULL,

    -- min_refresh_delta_blocks is the δ_min fee floor on refresh
    -- liquidity, expressed in blocks. Refresh liquidity fees are
    -- priced against max(δ, min_refresh_delta_blocks) to prevent
    -- a lazy-refresh bypass. See docs/fee-model.md "Fee floor δ_min".
    min_refresh_delta_blocks INTEGER NOT NULL,

    -- min_viable_policy is "reject" or "warn".
    min_viable_policy TEXT NOT NULL,

    -- min_viable_pct is the max fee-to-amount ratio.
    min_viable_pct INTEGER NOT NULL,

    -- created_at is the Unix timestamp of the change.
    created_at BIGINT NOT NULL
);

CREATE UNIQUE INDEX idx_forfeit_infos_outpoint
	ON round_forfeit_infos(outpoint_hash, outpoint_index);

CREATE INDEX idx_forfeit_infos_round
	ON round_forfeit_infos(round_id);

CREATE INDEX idx_indexer_receive_scripts_script
    ON indexer_receive_scripts(pk_script);

CREATE INDEX idx_indexer_vtxo_events_script_event
    ON indexer_vtxo_events(pk_script, event_id);

CREATE INDEX idx_ledger_created
    ON ledger_entries(created_at DESC);

CREATE INDEX idx_ledger_credit
    ON ledger_entries(credit_account);

CREATE INDEX idx_ledger_debit
    ON ledger_entries(debit_account);

CREATE INDEX idx_ledger_event_type
    ON ledger_entries(event_type);

CREATE INDEX idx_ledger_round
    ON ledger_entries(round_id);

CREATE INDEX idx_ledger_session
    ON ledger_entries(session_id);

CREATE UNIQUE INDEX idx_mailbox_envelopes_dedup
    ON mailbox_envelopes(recipient, msg_id);

CREATE INDEX idx_mailbox_envelopes_recipient_seq
    ON mailbox_envelopes(recipient, event_seq);

CREATE INDEX idx_oor_recipient_events_session_db_id
    ON oor_recipient_events(session_db_id);

CREATE INDEX idx_oor_sessions_state_updated
    ON oor_sessions(state, updated_at);

CREATE INDEX idx_round_connector_outputs_round
	ON round_connector_outputs(round_id);

CREATE INDEX idx_rounds_created_at
	ON rounds(created_at DESC);

CREATE INDEX idx_rounds_status
	ON rounds(status);

CREATE INDEX idx_rounds_txid
	ON rounds(commitment_txid);

CREATE INDEX idx_utxo_log_block
    ON wallet_utxo_log(block_height);

CREATE INDEX idx_utxo_log_classification
    ON wallet_utxo_log(classified_as);

CREATE INDEX idx_utxo_log_outpoint
    ON wallet_utxo_log(outpoint_hash, outpoint_index);

CREATE INDEX idx_utxo_log_source_id
    ON wallet_utxo_log(source_id)
    WHERE source_id IS NOT NULL;

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

    -- owner_pubkey is the optional compressed owner pubkey for standard Ark
    -- VTXO receive scripts.
    owner_pubkey BLOB,

    -- operator_pubkey is the optional compressed operator pubkey committed to
    -- the standard Ark VTXO receive script.
    operator_pubkey BLOB,

    -- exit_delay is the optional CSV delay committed to the standard Ark VTXO
    -- receive script.
    exit_delay BIGINT,

    PRIMARY KEY(principal_mailbox_id, pk_script)
);

CREATE TABLE indexer_vtxo_events (
    -- event_id is the global monotonic event cursor.
    event_id INTEGER PRIMARY KEY AUTOINCREMENT,

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
, value_sat BIGINT NOT NULL DEFAULT 0, round_id TEXT NOT NULL DEFAULT '', batch_expiry_height INTEGER NOT NULL DEFAULT 0, relative_expiry INTEGER NOT NULL DEFAULT 0, origin TEXT NOT NULL DEFAULT '', commitment_txid BLOB);

CREATE TABLE ledger_entries (
    -- entry_id is the monotonically increasing primary key.
    entry_id INTEGER PRIMARY KEY AUTOINCREMENT,

    -- debit_account is the account being debited.
    debit_account TEXT NOT NULL
        REFERENCES accounts(account_id),

    -- credit_account is the account being credited.
    credit_account TEXT NOT NULL
        REFERENCES accounts(account_id),

    -- amount_sat is the transaction amount in satoshis. Must
    -- be strictly positive — zero-amount entries are rejected at
    -- the schema layer because they represent no economic event
    -- and would pollute audit counts.
    amount_sat BIGINT NOT NULL CHECK (amount_sat > 0),

    -- round_id optionally links this entry to a specific round.
    -- Round-scoped events (boarding, refresh, offboard, mining,
    -- capital_committed, round_sweep) set this; OOR and
    -- external-wallet events do not.
    round_id BLOB,

    -- session_id optionally links this entry to a specific OOR
    -- session (32-byte identifier). OOR-scoped events set this;
    -- round-scoped events do not.
    session_id BLOB,

    -- idempotency_key is a caller-supplied opaque identifier used
    -- to make at-least-once mailbox replay a silent no-op. When
    -- set, the partial unique index below rejects a duplicate
    -- (key, event_type, debit, credit) insert; the sqlc query
    -- uses ON CONFLICT DO NOTHING so a redelivered message
    -- resolves to zero rows inserted instead of a constraint
    -- violation. Nullable because some historic callers predate
    -- idempotency and some events (external_deposit keyed on
    -- outpoint) naturally derive the key from another source.
    idempotency_key BLOB,

    -- event_type classifies the ledger entry for filtering.
    event_type TEXT NOT NULL
        REFERENCES ledger_event_types(event_type),

    -- description is a human-readable note about the entry.
    description TEXT NOT NULL,

    -- created_at is the Unix timestamp when this entry was
    -- recorded.
    created_at BIGINT NOT NULL,

    -- A self-transfer (debit_account = credit_account) is a
    -- silent no-op: the +amount and -amount contributions to the
    -- same account cancel in any balance aggregation, so even the
    -- sum-to-zero invariant cannot detect it. Reject at the schema
    -- layer so a buggy caller cannot pollute the audit log.
    CHECK (debit_account <> credit_account),

    -- round_id and session_id are mutually exclusive: an event
    -- belongs to at most one of them. Events without a round or
    -- session context (external deposits/withdrawals) leave both
    -- null.
    CHECK (round_id IS NULL OR session_id IS NULL)
);

CREATE TABLE ledger_event_types (
    event_type TEXT PRIMARY KEY
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
    -- target. AUTOINCREMENT is required so SQLite never reuses ROWIDs of
    -- deleted (acked) envelopes; without it, a freshly garbage-collected
    -- recipient mailbox would assign a new envelope a sequence at or below
    -- the client's persisted ack cursor, hiding the envelope on the next
    -- pull.
    event_seq INTEGER PRIMARY KEY AUTOINCREMENT,

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
    id INTEGER PRIMARY KEY AUTOINCREMENT,

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

CREATE TABLE round_connector_outputs (
	-- round_id links to the parent round.
	round_id BLOB NOT NULL,

	-- output_index is the FinalTx output index holding a connector.
	output_index INTEGER NOT NULL,

	PRIMARY KEY (round_id, output_index),
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
	updated_at BIGINT NOT NULL, change_output_idx INTEGER NOT NULL DEFAULT -1,

	FOREIGN KEY (status) REFERENCES round_statuses(status)
);

CREATE UNIQUE INDEX uniq_ledger_idempotency
    ON ledger_entries(
        idempotency_key, event_type, debit_account, credit_account
    )
    WHERE idempotency_key IS NOT NULL;

CREATE TABLE utxo_classifications (
    classification TEXT PRIMARY KEY
);

CREATE TABLE utxo_events (
    event TEXT PRIMARY KEY
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

	-- policy_template is the semantic arkscript policy for this VTXO.
	-- The server still indexes by pk_script, but this semantic form is the
	-- authoritative ownership/policy representation.
	policy_template BLOB,

	-- cosigner_key is the 33-byte compressed public key of the VTXO owner.
	--
	-- This key is always required for spend path reconstruction.
	cosigner_key BLOB NOT NULL,

	-- status tracks VTXO lifecycle (pending, live, in_flight,
	-- forfeited, spent, unrolled_by_client, expired).
	status TEXT NOT NULL DEFAULT 'pending',

	-- lock_owner_kind identifies who owns the in-flight lock.
	-- NULL when unlocked.
	lock_owner_kind TEXT,

	-- lock_owner_id identifies the lock owner instance within the kind.
	-- NULL when unlocked.
	lock_owner_id BLOB, batch_expiry INTEGER,

	PRIMARY KEY (outpoint_hash, outpoint_index),
	FOREIGN KEY (round_id) REFERENCES rounds(round_id),
	FOREIGN KEY (status) REFERENCES vtxo_statuses(status),
	CHECK (lock_owner_kind IS NULL OR lock_owner_kind IN ('round', 'oor')),
	CHECK ((lock_owner_kind IS NULL) = (lock_owner_id IS NULL)),
	CHECK ((status = 'in_flight') = (lock_owner_kind IS NOT NULL))
);

CREATE TABLE wallet_utxo_log (
    entry_id INTEGER PRIMARY KEY AUTOINCREMENT,

    -- outpoint_hash is the transaction hash (32 bytes).
    outpoint_hash BLOB NOT NULL,

    -- outpoint_index is the output index.
    outpoint_index INTEGER NOT NULL,

    -- amount_sat is the UTXO value.
    amount_sat BIGINT NOT NULL,

    -- event is 'created' or 'spent'.
    event TEXT NOT NULL
        REFERENCES utxo_events(event),

    -- block_height is the block where this change occurred.
    block_height INTEGER NOT NULL,

    -- classified_as categorizes the UTXO event.
    classified_as TEXT NOT NULL
        REFERENCES utxo_classifications(classification),

    -- created_at is the Unix timestamp when this entry was
    -- recorded.
    created_at BIGINT NOT NULL, source_id BLOB,

    -- (outpoint, event) is unique across the log. The diff loop
    -- that writes these rows runs every block and may retry after
    -- a crash, so the audit sink uses ON CONFLICT DO NOTHING to
    -- make replay a silent no-op. Note that (hash, index) alone
    -- is not unique: a single outpoint can appear once with
    -- event='created' and again with event='spent' over its
    -- lifetime.
    UNIQUE (outpoint_hash, outpoint_index, event)
);

