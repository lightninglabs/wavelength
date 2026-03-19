-- VTXO tables migration.
-- This migration creates tables for VTXO lifecycle management and forfeit tracking.

-- VTXO status enum table.
CREATE TABLE IF NOT EXISTS vtxo_statuses (
	status TEXT PRIMARY KEY
);

-- Populate the possible VTXO statuses.
-- VTXOs follow a state machine: pending → live → in_flight → (forfeited|spent).
INSERT INTO vtxo_statuses (status) VALUES
	('pending'),    -- Commitment tx broadcast but not yet confirmed
	('live'),       -- Commitment tx confirmed, VTXO is spendable
	('in_flight'),  -- Reserved for a spend operation (forfeit or out-of-round)
	('forfeited'),  -- Forfeited back to operator
	('spent')       -- Spent in out-of-round transaction
ON CONFLICT DO NOTHING;

-- VTXOs table.
-- Virtual Transaction Outputs created in rounds or virtual transactions.
-- Tracks VTXO lifecycle, status, and locking for concurrent round safety.
CREATE TABLE IF NOT EXISTS vtxos (
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

	-- exit_delay is the CSV delay committed to the unilateral timeout
	-- path of the VTXO.
	exit_delay INTEGER NOT NULL,

	-- pk_script is the P2TR script for the VTXO output.
	pk_script BLOB NOT NULL,

	-- owner_key is the 33-byte compressed public key of the VTXO owner.
	--
	-- This key is required for spend path reconstruction.
	owner_key BLOB NOT NULL,

	-- operator_key is the 33-byte compressed operator public key required
	-- by the VTXO collaborative spend path.
	operator_key BLOB NOT NULL,

	-- operator_key_family and operator_key_index identify the wallet key
	-- that must sign the collaborative spend path for this VTXO.
	operator_key_family INTEGER NOT NULL,
	operator_key_index INTEGER NOT NULL,

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

-- Index on round_id for listing VTXOs by round.
CREATE INDEX IF NOT EXISTS idx_vtxos_round
	ON vtxos(round_id);

-- Index on status for filtering by lifecycle state.
CREATE INDEX IF NOT EXISTS idx_vtxos_status
	ON vtxos(status);

-- Partial index on lock owner for tracking in-flight VTXOs.
-- Only indexes rows where lock_owner_id IS NOT NULL for efficiency.
CREATE INDEX IF NOT EXISTS idx_vtxos_locked
	ON vtxos(lock_owner_kind, lock_owner_id) WHERE lock_owner_id IS NOT NULL;

-- Forfeit info table.
-- Stores ForfeitInfo metadata (map[wire.OutPoint]*ForfeitInfo) for each round.
-- Records how VTXOs were forfeited and the connector leaf assignments.
CREATE TABLE IF NOT EXISTS round_forfeit_infos (
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

-- Index on round_id for listing forfeit infos by round.
CREATE INDEX IF NOT EXISTS idx_forfeit_infos_round
	ON round_forfeit_infos(round_id);

-- Ensure a given outpoint can only be forfeited once across all rounds.
CREATE UNIQUE INDEX IF NOT EXISTS idx_forfeit_infos_outpoint
	ON round_forfeit_infos(outpoint_hash, outpoint_index);
