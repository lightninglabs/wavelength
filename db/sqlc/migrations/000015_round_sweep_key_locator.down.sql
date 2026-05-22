-- Revert 000015_round_sweep_key_locator.

ALTER TABLE rounds DROP COLUMN sweep_key_family;
ALTER TABLE rounds DROP COLUMN sweep_key_index;
