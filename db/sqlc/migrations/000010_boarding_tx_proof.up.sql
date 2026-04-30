-- Adds the SPV TxProof column to the wallet's boarding_intents table so a
-- proof built when a UTXO first confirms survives a daemon restart and can be
-- replayed to the round actor (and onward to the server) without rebuilding it
-- from chain. The wire format matches round_boarding_intents.tx_proof: a TLV
-- encoding produced by lib/types.SerializeTxProof.
--
-- The column is nullable so legacy rows written before this migration keep
-- decoding cleanly (None on read); fresh inserts populate it via
-- domainIntentToInsertParams.
ALTER TABLE boarding_intents ADD COLUMN tx_proof BLOB;
