-- VTXO status tracking migration.
-- This migration adds columns for VTXO lifecycle status management and forfeit
-- transaction tracking for crash recovery.

-- Add status column for VTXO lifecycle tracking.
-- Status values map to vtxo.VTXOStatus enum:
--   0 = Live (default)
--   1 = RefreshRequested
--   2 = Forfeiting
--   3 = Forfeited
--   4 = Spent
--   5 = Expiring
--   6 = Failed
ALTER TABLE vtxos ADD COLUMN status INTEGER NOT NULL DEFAULT 0;

-- Add forfeit tracking columns for crash recovery.
-- When a VTXO enters the forfeiting flow, we persist the forfeit round and
-- transaction so we can recover state after a crash.

-- forfeit_round_id is the round in which this VTXO is being forfeited.
-- NULL unless VTXO is in Forfeiting or Forfeited status.
ALTER TABLE vtxos ADD COLUMN forfeit_round_id TEXT;

-- forfeit_tx is the serialized wire.MsgTx (binary) of the forfeit transaction.
-- Persisted when entering Forfeiting state for crash recovery.
ALTER TABLE vtxos ADD COLUMN forfeit_tx BLOB;

-- forfeit_txid is the 32-byte hash of the forfeit transaction.
-- Set when the forfeit is confirmed (transition to Forfeited state).
ALTER TABLE vtxos ADD COLUMN forfeit_txid BLOB;

-- Add replacement tracking for refresh auditing.
-- When a VTXO is forfeited and replaced by a new VTXO in the refresh round,
-- we link them for audit trail and debugging purposes.

-- replaced_by_hash is the outpoint hash of the replacement VTXO.
ALTER TABLE vtxos ADD COLUMN replaced_by_hash BLOB;

-- replaced_by_index is the outpoint index of the replacement VTXO.
ALTER TABLE vtxos ADD COLUMN replaced_by_index INTEGER;

-- Index on status for efficient status-based queries (ListLiveVTXOs, etc.).
CREATE INDEX IF NOT EXISTS idx_vtxos_status ON vtxos(status);
