-- Add round metadata columns to the VTXO event feed so clients that
-- reconcile via ListVTXOEventsByScripts get the same fields as the
-- transient mailbox push notification.

ALTER TABLE indexer_vtxo_events ADD COLUMN value_sat BIGINT NOT NULL DEFAULT 0;
ALTER TABLE indexer_vtxo_events ADD COLUMN round_id TEXT NOT NULL DEFAULT '';
ALTER TABLE indexer_vtxo_events ADD COLUMN batch_expiry_height INTEGER NOT NULL DEFAULT 0;
ALTER TABLE indexer_vtxo_events ADD COLUMN relative_expiry INTEGER NOT NULL DEFAULT 0;
ALTER TABLE indexer_vtxo_events ADD COLUMN origin TEXT NOT NULL DEFAULT '';
ALTER TABLE indexer_vtxo_events ADD COLUMN commitment_txid BLOB;
