ALTER TABLE owned_receive_scripts
    ADD COLUMN idempotency_key TEXT;

ALTER TABLE owned_receive_scripts
    ADD COLUMN registration_label TEXT;

ALTER TABLE owned_receive_scripts
    ADD COLUMN registration_expires_at BIGINT;

ALTER TABLE owned_receive_scripts
    ADD COLUMN registration_rpc_key TEXT;

ALTER TABLE owned_receive_scripts
    ADD COLUMN registration_completed_at BIGINT;

CREATE UNIQUE INDEX idx_owned_receive_scripts_idempotency_key
    ON owned_receive_scripts(idempotency_key)
    WHERE idempotency_key IS NOT NULL;
