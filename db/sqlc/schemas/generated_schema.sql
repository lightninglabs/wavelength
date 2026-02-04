CREATE TABLE chain_info (
    id INTEGER PRIMARY KEY,
    chain_name TEXT NOT NULL UNIQUE,
    genesis_hash BLOB NOT NULL
);
CREATE UNIQUE INDEX idx_forfeit_infos_outpoint
	ON round_forfeit_infos(outpoint_hash, outpoint_index);

CREATE INDEX idx_forfeit_infos_round
	ON round_forfeit_infos(round_id);
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
	value INTEGER NOT NULL,
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
	amount INTEGER NOT NULL,

	-- Optional fields populated after signing.
	signature BLOB,
	final_key BLOB,

	PRIMARY KEY (round_id, batch_output_index, node_id),
	FOREIGN KEY (round_id, batch_output_index, parent_node_id)
		REFERENCES vtxo_tree_nodes(round_id, batch_output_index, node_id)
		ON DELETE CASCADE
);
