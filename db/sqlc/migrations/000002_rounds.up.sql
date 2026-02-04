-- Round tables migration.
-- This migration creates tables for server-side round FSM persistence.

-- Enum-like table for round lifecycle states.
CREATE TABLE IF NOT EXISTS round_statuses (
	status TEXT PRIMARY KEY
);

-- Populate the possible round statuses.
-- Server rounds have a simpler lifecycle than client rounds.
INSERT INTO round_statuses (status) VALUES
	('pending'),    -- Finalized, commitment tx broadcast, awaiting confirmation
	('confirmed')   -- Commitment tx confirmed on-chain, VTXOs now live
ON CONFLICT DO NOTHING;

-- Main rounds table.
-- Rounds coordinate client registrations (boarding, VTXOs, forfeits) into
-- a single commitment transaction. Persisted after finalization for restart recovery.
CREATE TABLE IF NOT EXISTS rounds (
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

-- Index on status for ListPendingRounds queries.
CREATE INDEX IF NOT EXISTS idx_rounds_status
	ON rounds(status);

-- Index on commitment_txid for confirmation callback lookups.
CREATE INDEX IF NOT EXISTS idx_rounds_txid
	ON rounds(commitment_txid);

-- Index on created_at for chronological queries.
CREATE INDEX IF NOT EXISTS idx_rounds_created_at
	ON rounds(created_at DESC);

-- VTXO tree tables (normalized/recursive storage).
-- Stores VTXO trees in a relational format that supports recursive queries.
-- Each batch output in the commitment tx has a corresponding VTXO tree.

-- Table for tracking VTXO trees.
-- Each batch output in the commitment tx has one VTXO tree.
-- This is just a marker table - the actual tree structure is stored in
-- vtxo_tree_nodes.
CREATE TABLE IF NOT EXISTS round_vtxo_tree (
	-- round_id links to the parent round.
	round_id BLOB NOT NULL,

	-- batch_output_index is the commitment tx output index that roots this tree.
	batch_output_index INTEGER NOT NULL,

	PRIMARY KEY (round_id, batch_output_index),
	FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE
);

-- Table for storing individual nodes in VTXO trees.
-- Each node represents a single transaction in the tree hierarchy.
CREATE TABLE IF NOT EXISTS vtxo_tree_nodes (
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

-- Index for querying by parent (traversing down the tree).
CREATE INDEX IF NOT EXISTS idx_vtxo_tree_nodes_parent
	ON vtxo_tree_nodes(round_id, batch_output_index, parent_node_id);

-- Index for querying by depth (breadth-first traversal).
CREATE INDEX IF NOT EXISTS idx_vtxo_tree_nodes_depth
	ON vtxo_tree_nodes(round_id, batch_output_index, depth);

-- Index for querying leaf nodes.
CREATE INDEX IF NOT EXISTS idx_vtxo_tree_nodes_leaves
	ON vtxo_tree_nodes(round_id, batch_output_index, is_leaf)
	WHERE is_leaf = 1;

-- Table for storing outputs of each node transaction.
CREATE TABLE IF NOT EXISTS vtxo_tree_node_outputs (
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

-- Table for storing cosigner keys efficiently (normalized).
-- This allows querying "find all nodes where this key is a cosigner".
CREATE TABLE IF NOT EXISTS vtxo_tree_cosigners (
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

-- Index for finding nodes by cosigner key.
CREATE INDEX IF NOT EXISTS idx_vtxo_tree_cosigners_key
	ON vtxo_tree_cosigners(cosigner_key, round_id, batch_output_index);

-- Connector tree descriptors table.
-- Stores ConnectorTreeDescriptor metadata for forfeit transaction construction.
-- These describe connector outputs and their leaf structure.
CREATE TABLE IF NOT EXISTS round_connector_descriptors (
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

-- Client registrations table.
-- Stores ClientRegistration data (map[ClientID]*ClientRegistration) for each round.
-- This includes boarding inputs, leave outputs, VTXO descriptors, and forfeit inputs.
CREATE TABLE IF NOT EXISTS round_client_registrations (
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
