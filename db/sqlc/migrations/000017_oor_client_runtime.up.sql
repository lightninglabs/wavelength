CREATE TABLE IF NOT EXISTS oor_client_sessions (
    session_id        BLOB    PRIMARY KEY,
    direction         TEXT    NOT NULL CHECK (direction IN ('outgoing', 'incoming')),

    state             TEXT    NOT NULL CHECK (state IN (
        'awaiting_ark_signatures',
        'awaiting_submit_accepted',
        'awaiting_checkpoint_signatures',
        'awaiting_finalize_accepted',
        'awaiting_local_vtxo_update',
        'completed',
        'failed',
        'receive_resolving',
        'receive_notified',
        'receive_awaiting_ack',
        'receive_completed'
    )),

    idempotency_key   TEXT,
    retry_after       BIGINT,
    retry_reason      TEXT,
    fail_reason       TEXT,

    created_at        BIGINT  NOT NULL,
    updated_at        BIGINT  NOT NULL,
    completed_at      BIGINT,

    UNIQUE (direction, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_oor_client_sessions_active
    ON oor_client_sessions(direction, updated_at)
    WHERE completed_at IS NULL;

CREATE TABLE IF NOT EXISTS oor_client_inputs (
    session_id           BLOB    NOT NULL REFERENCES oor_client_sessions(session_id) ON DELETE CASCADE,
    input_index          INTEGER NOT NULL,
    outpoint_hash        BLOB    NOT NULL,
    outpoint_index       INTEGER NOT NULL,
    amount_sat           BIGINT  NOT NULL,
    pk_script            BLOB    NOT NULL,
    client_key_family    INTEGER NOT NULL,
    client_key_index     INTEGER NOT NULL,
    client_pub_key       BLOB    NOT NULL,
    operator_pub_key     BLOB    NOT NULL,
    exit_delay           INTEGER NOT NULL,
    vtxo_policy_template BLOB,
    owner_leaf_script    BLOB,
    owner_leaf_policy    BLOB,
    spend_witness_script BLOB,
    spend_control_block  BLOB,
    condition_witness    BLOB,
    required_sequence    INTEGER,
    required_locktime    INTEGER,
    PRIMARY KEY (session_id, input_index)
);

CREATE TABLE IF NOT EXISTS oor_client_recipients (
    session_id           BLOB    NOT NULL REFERENCES oor_client_sessions(session_id) ON DELETE CASCADE,
    output_index         INTEGER NOT NULL,
    pk_script            BLOB    NOT NULL,
    value_sat            BIGINT  NOT NULL,
    vtxo_policy_template BLOB,
    PRIMARY KEY (session_id, output_index)
);

CREATE TABLE IF NOT EXISTS oor_client_checkpoints (
    session_id       BLOB    NOT NULL REFERENCES oor_client_sessions(session_id) ON DELETE CASCADE,
    checkpoint_index INTEGER NOT NULL,
    phase            TEXT    NOT NULL CHECK (phase IN ('unsigned', 'cosigned', 'finalized')),
    checkpoint_psbt  BLOB    NOT NULL,
    created_at       BIGINT  NOT NULL,
    updated_at       BIGINT  NOT NULL,
    PRIMARY KEY (session_id, checkpoint_index, phase)
);

CREATE TABLE IF NOT EXISTS oor_client_ark_artifacts (
    session_id BLOB    NOT NULL REFERENCES oor_client_sessions(session_id) ON DELETE CASCADE,
    phase      TEXT    NOT NULL CHECK (phase IN (
        'unsigned', 'ark_signed', 'accepted', 'finalized_context'
    )),
    ark_psbt   BLOB    NOT NULL,
    created_at BIGINT  NOT NULL,
    updated_at BIGINT  NOT NULL,
    PRIMARY KEY (session_id, phase)
);

CREATE TABLE IF NOT EXISTS oor_client_incoming_hints (
    session_id          BLOB   NOT NULL REFERENCES oor_client_sessions(session_id) ON DELETE CASCADE,
    recipient_pk_script BLOB   NOT NULL,
    recipient_event_id  BIGINT NOT NULL,
    created_at          BIGINT NOT NULL,
    updated_at          BIGINT NOT NULL,
    UNIQUE (recipient_pk_script, recipient_event_id)
);

CREATE TABLE IF NOT EXISTS oor_client_incoming_metadata (
    session_id      BLOB    NOT NULL REFERENCES oor_client_sessions(session_id) ON DELETE CASCADE,
    output_index    INTEGER NOT NULL,
    round_id        BLOB,
    chain_depth     INTEGER,
    batch_expiry    INTEGER,
    operator_pubkey BLOB,
    ancestry_blob   BLOB,
    metadata_blob   BLOB,
    created_at      BIGINT  NOT NULL,
    updated_at      BIGINT  NOT NULL,
    PRIMARY KEY (session_id, output_index)
);

CREATE TABLE IF NOT EXISTS oor_client_effects (
    id              TEXT    PRIMARY KEY,
    session_id      BLOB    NOT NULL REFERENCES oor_client_sessions(session_id) ON DELETE CASCADE,
    direction       TEXT    NOT NULL CHECK (direction IN ('outgoing', 'incoming')),

    effect_type     TEXT    NOT NULL CHECK (effect_type IN (
        'request_ark_signatures',
        'send_submit_package',
        'request_checkpoint_signatures',
        'send_finalize_package',
        'mark_inputs_spent',
        'persist_outgoing_package',
        'query_incoming_transfer',
        'notify_incoming_transfer',
        'query_incoming_metadata',
        'materialize_incoming_vtxos',
        'send_incoming_ack'
    )),

    status          TEXT    NOT NULL CHECK (status IN ('pending', 'claimed', 'done', 'dead')),
    idempotency_key TEXT    NOT NULL UNIQUE,

    attempts        INTEGER NOT NULL DEFAULT 0,
    max_attempts    INTEGER NOT NULL DEFAULT 10,
    next_attempt_at BIGINT  NOT NULL,
    claim_owner     TEXT,
    claim_token     TEXT,
    claim_until     BIGINT,
    last_error      TEXT,

    created_at      BIGINT  NOT NULL,
    updated_at      BIGINT  NOT NULL,
    done_at         BIGINT
);

CREATE INDEX IF NOT EXISTS idx_oor_client_effects_due
    ON oor_client_effects(status, next_attempt_at, created_at);
