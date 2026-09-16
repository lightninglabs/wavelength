ALTER TABLE pending_board_intents ADD COLUMN service_authorization BLOB;
ALTER TABLE rounds ADD COLUMN service_authorization BLOB;

CREATE TABLE service_operations (
    operation_id BLOB PRIMARY KEY NOT NULL CHECK (length(operation_id) = 32),
    authorization_blob BLOB NOT NULL,
    round_id TEXT,
    deadline_unix BIGINT NOT NULL DEFAULT 0,
    active BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE TABLE service_operation_inputs (
    operation_id BLOB NOT NULL REFERENCES service_operations(operation_id),
    txid BLOB NOT NULL CHECK (length(txid) = 32),
    output_index BIGINT NOT NULL,
    is_forfeit BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (operation_id, txid, output_index)
);
CREATE INDEX service_operation_input_lookup
    ON service_operation_inputs (txid, output_index);
