-- mailboxrpcclient durable store migration.
--
-- This migration creates a small persistence schema for mailboxrpcclient. It
-- is intentionally separate from the durable actor mailbox tables: this store
-- exists to make cursor-based AckUpTo safe for RPC responses by persisting
-- pulled responses before advancing the remote cursor.

CREATE TABLE IF NOT EXISTS mailboxrpcclient_cursors (
    -- mailbox_id is the client mailbox id (recipient id used in Pull).
    mailbox_id TEXT PRIMARY KEY,

    -- cursor is the next event_seq the client should request from Pull.
    cursor BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS mailboxrpcclient_responses (
    -- mailbox_id is the client mailbox id.
    mailbox_id TEXT NOT NULL,

    -- correlation_id matches mailboxpb.Envelope.rpc.correlation_id.
    correlation_id TEXT NOT NULL,

    -- payload contains the protobuf message bytes stored in Any.Value.
    payload BLOB NOT NULL,

    PRIMARY KEY (mailbox_id, correlation_id)
);

