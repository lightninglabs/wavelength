-- commitment_height is the on-chain confirmation height of the commitment tx
-- anchoring this ancestry fragment. It is the tightest sound floor for the
-- unroller's proof-node confirmation-watch height hint (nothing in a VTXO's
-- proof graph confirms before its commitment tx).
--
-- Added as a forward migration (rather than folded into 000004_vtxos) so
-- existing deployments gain the column without a schema reset. DEFAULT 0 means
-- unknown: rows persisted before this column existed, and rows whose producer
-- did not populate it, read back as 0 and make the unroller fall back to a
-- bounded lookback floor.
ALTER TABLE vtxo_ancestry_paths
    ADD COLUMN commitment_height INTEGER NOT NULL DEFAULT 0;
