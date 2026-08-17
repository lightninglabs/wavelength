DROP INDEX IF EXISTS idx_owned_receive_scripts_idempotency_key;

ALTER TABLE owned_receive_scripts DROP COLUMN registration_expires_at;
ALTER TABLE owned_receive_scripts DROP COLUMN registration_completed_at;
ALTER TABLE owned_receive_scripts DROP COLUMN registration_rpc_key;
ALTER TABLE owned_receive_scripts DROP COLUMN registration_label;
ALTER TABLE owned_receive_scripts DROP COLUMN idempotency_key;
