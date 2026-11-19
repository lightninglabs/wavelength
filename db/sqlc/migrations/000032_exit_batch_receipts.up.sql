-- NULL claims the ID for one submit before it reserves any inputs.
-- A closed request (empty receipt) can never enqueue a cooperative exit.
-- An admitted request stores the exact result with the accepted round state.
CREATE TABLE exit_batch_receipts (
    request_id TEXT PRIMARY KEY NOT NULL,
    receipt BLOB
);
