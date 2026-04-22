-- Round attribution persistence migration.
--
-- Adds persistent storage for the two FSM fields the ledger classifier
-- needs on restart: ChangeOutputIdx (the PSBT output index where FundPsbt
-- put the wallet change, or -1 when no change output was added) and
-- ConnectorOutputIndices (the set of FinalTx output indices that hold
-- operator-controlled connector outputs). Without these on disk, a
-- rounds-actor restart between round finalization and block confirmation
-- reloads the round with ChangeOutputIdx=-1 and no connector indices, so
-- the UTXO diff classifier mis-attributes the change output as
-- external_deposit on top of the round's RecordCapitalCommitted ledger
-- leg -- exactly the double-count the classifier exists to prevent.

-- Stores the wallet change output index. Default -1 mirrors the
-- "no change produced" sentinel on FinalizedState.ChangeOutputIdx;
-- existing pre-migration rows will read back as -1 and fall back to
-- the grace-window reconcile, matching pre-migration behavior.
ALTER TABLE rounds
	ADD COLUMN change_output_idx INTEGER NOT NULL DEFAULT -1;

-- Stores connector output indices as a one-to-many side table. Connector
-- outputs are dust-valued operator-controlled outputs spent by forfeit
-- transactions later; attributing them alongside the wallet change keeps
-- the classifier from double-booking external_deposit on round-minted
-- dust.
CREATE TABLE IF NOT EXISTS round_connector_outputs (
	-- round_id links to the parent round.
	round_id BLOB NOT NULL,

	-- output_index is the FinalTx output index holding a connector.
	output_index INTEGER NOT NULL,

	PRIMARY KEY (round_id, output_index),
	FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE
);

-- Index for bulk loads on restart (LoadPendingRounds walks every
-- pending round and pulls its connector set).
CREATE INDEX IF NOT EXISTS idx_round_connector_outputs_round
	ON round_connector_outputs(round_id);
