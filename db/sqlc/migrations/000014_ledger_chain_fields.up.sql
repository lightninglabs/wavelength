-- Add first-class chain metadata columns to ledger_entries so history reads do
-- not need to decode wallet UTXO idempotency keys on every query.
ALTER TABLE ledger_entries ADD COLUMN chain_txid BLOB;
ALTER TABLE ledger_entries ADD COLUMN chain_vout INTEGER;
ALTER TABLE ledger_entries ADD COLUMN confirmation_height INTEGER;

CREATE INDEX IF NOT EXISTS idx_client_ledger_chain_txid
    ON ledger_entries(chain_txid)
    WHERE chain_txid IS NOT NULL;
