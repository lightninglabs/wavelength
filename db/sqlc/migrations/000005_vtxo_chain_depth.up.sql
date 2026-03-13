-- Add chain_depth to vtxos table. This tracks the number of OOR checkpoint
-- hops between a VTXO and the most recent on-chain commitment. Round-created
-- VTXOs have chain_depth 0. Existing rows default to 0 because they are
-- either round-created or have unknown historical OOR depth.
ALTER TABLE vtxos ADD COLUMN chain_depth INTEGER NOT NULL DEFAULT 0;
