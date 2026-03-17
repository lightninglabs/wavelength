-- Mailbox envelope persistence schema.
--
-- This replaces the in-memory mailbox.MemoryStore with durable storage for
-- envelope transport between server and client connectors. Each mailbox is
-- identified by a recipient string and envelopes are ordered by a monotonic
-- event sequence number.
--
-- NOTE: This schema is written to be compatible with both sqlite and postgres
-- backends.

-- mailbox_envelopes stores serialized proto envelopes keyed by recipient
-- and ordered by event_seq. The ingress loop pulls envelopes starting from
-- a cursor, and AckUpTo garbage-collects acknowledged envelopes.
CREATE TABLE IF NOT EXISTS mailbox_envelopes (
    -- event_seq is the monotonically increasing sequence number assigned
    -- on append. It serves as both the primary key and the pull cursor
    -- target.
    event_seq INTEGER PRIMARY KEY,

    -- recipient identifies the target mailbox (e.g., "client-<id>" or
    -- "server-for-<id>").
    recipient TEXT NOT NULL,

    -- msg_id is the stable message identifier for deduplication.
    msg_id TEXT NOT NULL,

    -- envelope is the proto-serialized mailboxpb.Envelope bytes.
    envelope BLOB NOT NULL,

    -- created_at is the unix nano timestamp when the envelope was
    -- appended.
    created_at BIGINT NOT NULL
);

-- Index for Pull queries: find envelopes for a recipient starting at a
-- cursor position.
CREATE INDEX IF NOT EXISTS idx_mailbox_envelopes_recipient_seq
    ON mailbox_envelopes(recipient, event_seq);

-- Index for deduplication: prevent duplicate msg_id per recipient.
CREATE UNIQUE INDEX IF NOT EXISTS idx_mailbox_envelopes_dedup
    ON mailbox_envelopes(recipient, msg_id);

-- mailbox_ack_cursors tracks the ack watermark per recipient. Envelopes
-- with event_seq < ack_cursor have been processed and can be garbage
-- collected.
CREATE TABLE IF NOT EXISTS mailbox_ack_cursors (
    -- recipient is the mailbox identifier.
    recipient TEXT PRIMARY KEY,

    -- ack_cursor is the next expected event sequence number. All
    -- envelopes with event_seq < ack_cursor are considered acknowledged.
    ack_cursor BIGINT NOT NULL DEFAULT 0
);
