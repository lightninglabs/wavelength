-- Revert 000014_vtxo_inherited_batch_expiry.

ALTER TABLE vtxos DROP COLUMN batch_expiry;
