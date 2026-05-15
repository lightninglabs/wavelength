DROP INDEX IF EXISTS idx_client_ledger_chain_txid;
ALTER TABLE ledger_entries DROP COLUMN confirmation_height;
ALTER TABLE ledger_entries DROP COLUMN chain_vout;
ALTER TABLE ledger_entries DROP COLUMN chain_txid;
