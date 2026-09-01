ALTER TABLE ark_channels ADD COLUMN recovery_ready BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE ark_channels ADD COLUMN source_spent_outpoint_txid BLOB;
ALTER TABLE ark_channels ADD COLUMN source_spent_outpoint_index BIGINT;
ALTER TABLE ark_channels ADD COLUMN source_spending_txid BLOB;
