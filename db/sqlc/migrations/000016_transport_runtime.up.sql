CREATE TABLE IF NOT EXISTS mailbox_ingress_cursors (
    local_mailbox_id      TEXT PRIMARY KEY,
    remote_mailbox_id     TEXT NOT NULL,

    pull_cursor           BIGINT NOT NULL DEFAULT 0,
    dispatch_committed_to BIGINT NOT NULL DEFAULT 0,
    ack_target            BIGINT NOT NULL DEFAULT 0,
    ack_committed_to      BIGINT NOT NULL DEFAULT 0,

    last_pull_at          BIGINT,
    last_dispatch_at      BIGINT,
    last_ack_at           BIGINT,
    last_error            TEXT,

    created_at            BIGINT NOT NULL,
    updated_at            BIGINT NOT NULL,

    CHECK (pull_cursor >= 0),
    CHECK (dispatch_committed_to >= 0),
    CHECK (ack_target >= 0),
    CHECK (ack_committed_to >= 0),
    CHECK (ack_committed_to <= ack_target),
    CHECK (ack_target <= dispatch_committed_to)
);

CREATE INDEX IF NOT EXISTS idx_mailbox_ingress_remote
    ON mailbox_ingress_cursors(remote_mailbox_id);

CREATE TABLE IF NOT EXISTS mailbox_egress (
    id                TEXT PRIMARY KEY,
    connector         TEXT NOT NULL CHECK (connector IN ('serverconn', 'clientconn')),
    local_mailbox_id  TEXT NOT NULL,
    remote_mailbox_id TEXT NOT NULL,

    rpc_kind          TEXT NOT NULL CHECK (rpc_kind IN ('request', 'response', 'event')),
    service           TEXT NOT NULL,
    method            TEXT NOT NULL,
    correlation_id    TEXT,
    reply_to          TEXT,

    msg_id            TEXT NOT NULL,
    idempotency_key   TEXT NOT NULL,
    envelope          BLOB NOT NULL,

    status            TEXT NOT NULL CHECK (status IN ('pending', 'claimed', 'sent', 'dead')),
    attempts          INTEGER NOT NULL DEFAULT 0,
    max_attempts      INTEGER NOT NULL DEFAULT 10,
    next_attempt_at   BIGINT NOT NULL,
    claim_owner       TEXT,
    claim_token       TEXT,
    claim_until       BIGINT,
    last_error        TEXT,

    created_at        BIGINT NOT NULL,
    updated_at        BIGINT NOT NULL,
    sent_at           BIGINT,

    UNIQUE (remote_mailbox_id, msg_id),
    UNIQUE (connector, local_mailbox_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_mailbox_egress_due
    ON mailbox_egress(status, next_attempt_at, created_at);

CREATE INDEX IF NOT EXISTS idx_mailbox_egress_pair
    ON mailbox_egress(connector, local_mailbox_id, remote_mailbox_id, created_at);

CREATE INDEX IF NOT EXISTS idx_mailbox_egress_correlation
    ON mailbox_egress(correlation_id);
