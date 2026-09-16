DROP TABLE service_operation_inputs;
DROP TABLE service_operations;
ALTER TABLE rounds DROP COLUMN service_authorization;
ALTER TABLE pending_board_intents DROP COLUMN service_authorization;
