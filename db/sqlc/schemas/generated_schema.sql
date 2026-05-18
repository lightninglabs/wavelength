CREATE TABLE account_types (
    account_type TEXT PRIMARY KEY
);

CREATE TABLE accounts (
    account_id TEXT PRIMARY KEY,
    account_name TEXT NOT NULL,
    account_type TEXT NOT NULL
        REFERENCES account_types(account_type)
);

CREATE TABLE boarding_addresses (
    -- pk_script is the raw output script (P2TR script) and serves as the
    -- primary key since it uniquely identifies an address.
    pk_script BLOB PRIMARY KEY NOT NULL,

    -- address_string is the bech32m-encoded address for user display.
    address_string TEXT NOT NULL,

    -- client_pubkey is the serialized public key (33 bytes compressed) of the
    -- client used in the tapscript.
    client_pubkey BLOB NOT NULL,

    -- client_key_family is the BIP32 key family for the client key.
    client_key_family INTEGER NOT NULL,

    -- client_key_index is the BIP32 key index within the family.
    client_key_index INTEGER NOT NULL,

    -- operator_pubkey is the serialized public key (33 bytes compressed) of
    -- the operator used in the collaborative spend path.
    operator_pubkey BLOB NOT NULL,

    -- exit_delay is the CSV delay in blocks for the client's unilateral
    -- timeout path.
    exit_delay INTEGER NOT NULL,

    -- last_confirmed_height is the most recent block height at which we
    -- detected a confirmation for a UTXO at this address. Used for restart
    -- recovery to know from which height to resume monitoring.
    last_confirmed_height INTEGER NOT NULL DEFAULT 0,

    -- creation_time is the unix epoch timestamp when this address was created.
    creation_time BIGINT NOT NULL
);

CREATE TABLE boarding_intents (
    -- outpoint_hash and outpoint_index form the composite primary key,
    -- uniquely identifying the boarding UTXO.
    outpoint_hash BLOB NOT NULL,
    outpoint_index INTEGER NOT NULL,

    -- pk_script references the boarding_addresses table, linking this intent
    -- to its address.
    pk_script BLOB NOT NULL,

    -- amount is the value of the boarding UTXO in satoshis.
    amount BIGINT NOT NULL,

    -- conf_height is the block height at which this UTXO was confirmed.
    conf_height INTEGER NOT NULL,

    -- conf_hash is the block hash at which this UTXO was confirmed.
    conf_hash BLOB NOT NULL,

    -- conf_tx is the serialized confirmation transaction.
    conf_tx BLOB,

    -- status tracks the lifecycle of this intent.
    -- References the boarding_statuses table.
    status TEXT NOT NULL,

    -- creation_time is the unix epoch timestamp when this intent was created.
    creation_time BIGINT NOT NULL,

    -- last_update_time is the unix epoch timestamp of the last update.
    last_update_time BIGINT NOT NULL, tx_proof BLOB,

    PRIMARY KEY (outpoint_hash, outpoint_index),
    FOREIGN KEY (pk_script) REFERENCES boarding_addresses(pk_script),
    FOREIGN KEY (status) REFERENCES boarding_statuses(status_name)
);

CREATE TABLE boarding_statuses (
    id BIGINT PRIMARY KEY,
    status_name TEXT UNIQUE NOT NULL
);

CREATE TABLE boarding_sweep_inputs (
    txid BLOB NOT NULL,
    outpoint_hash BLOB NOT NULL,
    outpoint_index INTEGER NOT NULL,
    amount BIGINT NOT NULL,
    previous_status TEXT NOT NULL,
    status TEXT NOT NULL CHECK (
        status IN (
            'pending',
            'published',
            'spent',
            'external_spent',
            'failed'
        )
    ),
    spent_by_txid BLOB,
    spent_height INTEGER,
    last_update_time BIGINT NOT NULL,

    PRIMARY KEY (txid, outpoint_hash, outpoint_index),
    FOREIGN KEY (txid) REFERENCES boarding_sweeps(txid),
    FOREIGN KEY (previous_status) REFERENCES boarding_statuses(status_name),
    FOREIGN KEY (outpoint_hash, outpoint_index)
        REFERENCES boarding_intents(outpoint_hash, outpoint_index)
);

CREATE TABLE boarding_sweeps (
    txid BLOB PRIMARY KEY NOT NULL,
    raw_tx BLOB NOT NULL,
    destination_address TEXT NOT NULL,
    total_amount BIGINT NOT NULL,
    fee_amount BIGINT NOT NULL,
    fee_rate_sat_per_vbyte BIGINT NOT NULL,
    vbytes BIGINT NOT NULL,
    status TEXT NOT NULL CHECK (
        status IN (
            'pending',
            'published',
            'confirmed',
            'external_resolved',
            'failed'
        )
    ),
    created_height INTEGER NOT NULL,
    created_time BIGINT NOT NULL,
    published_time BIGINT,
    confirmed_height INTEGER,
    last_error TEXT
);

CREATE TABLE chain_info (
    id BIGINT PRIMARY KEY,
    chain_name TEXT NOT NULL UNIQUE,
    genesis_hash BLOB NOT NULL
);

CREATE TABLE client_round_agg_nonce_state (
    round_id TEXT NOT NULL,

    -- txid is the transaction this aggregate nonce belongs to.
    txid BLOB NOT NULL,

    -- agg_nonce is the 66-byte aggregate MuSig2 public nonce shared by the
    -- operator after collecting participant nonce submissions.
    agg_nonce BLOB NOT NULL CHECK(length(agg_nonce) = 66),

    creation_time BIGINT NOT NULL,
    last_update_time BIGINT NOT NULL,

    PRIMARY KEY (round_id, txid),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE
);

CREATE TABLE client_round_effects (
    id TEXT PRIMARY KEY NOT NULL,
    round_id TEXT NOT NULL,
    effect_type TEXT NOT NULL CHECK (effect_type IN (
        'send_nonces',
        'send_boarding_sigs',
        'send_partial_sigs',
        'request_vtxo_forfeit_sigs',
        'send_vtxo_forfeit_sigs',
        'register_confirmation'
    )),
    status TEXT NOT NULL CHECK (status IN (
        'pending', 'claimed', 'done', 'dead'
    )),
    idempotency_key TEXT NOT NULL UNIQUE,
    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 10,
    next_attempt_at BIGINT NOT NULL,
    claim_owner TEXT,
    claim_token TEXT,
    claim_until BIGINT,
    last_error TEXT,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    done_at BIGINT,

    FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE,
    CHECK (attempts >= 0),
    CHECK (max_attempts > 0),
    CHECK (next_attempt_at > 0),
    CHECK (
        (status = 'done' AND done_at IS NOT NULL) OR
        (status != 'done')
    )
);

CREATE TABLE client_round_forfeit_request_state (
    round_id TEXT NOT NULL,

    vtxo_outpoint_hash BLOB NOT NULL,
    vtxo_outpoint_index INTEGER NOT NULL,

    connector_outpoint_hash BLOB NOT NULL,
    connector_outpoint_index INTEGER NOT NULL,
    connector_pk_script BLOB NOT NULL,
    connector_amount BIGINT NOT NULL,
    vtxo_amount BIGINT NOT NULL,
    server_forfeit_pk_script BLOB NOT NULL,

    -- forfeit_spend is the optional encoded arkscript spend path override
    -- for custom policies. NULL means the VTXO actor should use its standard
    -- collaborative spend path.
    forfeit_spend BLOB,

    creation_time BIGINT NOT NULL,
    last_update_time BIGINT NOT NULL,

    PRIMARY KEY (round_id, vtxo_outpoint_hash, vtxo_outpoint_index),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE
);

CREATE TABLE client_round_forfeit_sig_state (
    round_id TEXT NOT NULL,

    -- vtxo_outpoint identifies the old VTXO being forfeited.
    vtxo_outpoint_hash BLOB NOT NULL,
    vtxo_outpoint_index INTEGER NOT NULL,

    -- forfeit_tx is the unsigned Bitcoin transaction built by the VTXO actor.
    forfeit_tx BLOB NOT NULL,

    -- client_sig is the 64-byte Schnorr signature for the VTXO input.
    client_sig BLOB NOT NULL CHECK(length(client_sig) = 64),

    -- spend_path is the canonical arkscript spend path used for the VTXO input.
    spend_path BLOB NOT NULL,

    creation_time BIGINT NOT NULL,
    last_update_time BIGINT NOT NULL,

    PRIMARY KEY (round_id, vtxo_outpoint_hash, vtxo_outpoint_index),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE
);

CREATE TABLE client_round_nonce_state (
    round_id TEXT NOT NULL,

    -- signing_key is the 33-byte compressed MuSig2 signing public key.
    signing_key BLOB NOT NULL,

    -- txid is the transaction this nonce signs.
    txid BLOB NOT NULL,

    -- pub_nonce is the 66-byte MuSig2 public nonce shared with the server.
    pub_nonce BLOB NOT NULL CHECK(length(pub_nonce) = 66),

    -- sec_nonce is the 97-byte MuSig2 secret nonce consumed during partial
    -- signing. It is sensitive wallet material.
    sec_nonce BLOB NOT NULL CHECK(length(sec_nonce) = 97),

    creation_time BIGINT NOT NULL,
    last_update_time BIGINT NOT NULL,

    PRIMARY KEY (round_id, signing_key, txid),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE
);

CREATE TABLE client_round_partial_sig_state (
    round_id TEXT NOT NULL,

    -- signing_key is the 33-byte compressed MuSig2 signing public key.
    signing_key BLOB NOT NULL,

    -- txid is the transaction this partial signature signs.
    txid BLOB NOT NULL,

    -- partial_sig is the 32-byte MuSig2 scalar signature fragment.
    partial_sig BLOB NOT NULL CHECK(length(partial_sig) = 32),

    creation_time BIGINT NOT NULL,
    last_update_time BIGINT NOT NULL,

    PRIMARY KEY (round_id, signing_key, txid),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE
);

CREATE TABLE client_round_pending_leave_quotes (
    round_id TEXT NOT NULL,
    quote_index INTEGER NOT NULL,
    pk_script BLOB NOT NULL,
    amount_sat BIGINT NOT NULL,

    PRIMARY KEY (round_id, quote_index),
    FOREIGN KEY (round_id) REFERENCES client_round_pending_quotes(round_id)
        ON DELETE CASCADE
);

CREATE TABLE client_round_pending_quotes (
    round_id TEXT PRIMARY KEY NOT NULL,
    quote_id BLOB NOT NULL CHECK(length(quote_id) = 32),
    seal_pass BIGINT NOT NULL,
    operator_fee_sat BIGINT NOT NULL,
    quote_expires_at BIGINT NOT NULL,
    reject_reason INTEGER NOT NULL,
    creation_time BIGINT NOT NULL,
    last_update_time BIGINT NOT NULL
);

CREATE TABLE client_round_pending_vtxo_quotes (
    round_id TEXT NOT NULL,
    quote_index INTEGER NOT NULL,
    pk_script BLOB NOT NULL,
    amount_sat BIGINT NOT NULL,
    recipient_key BLOB NOT NULL,

    PRIMARY KEY (round_id, quote_index),
    FOREIGN KEY (round_id) REFERENCES client_round_pending_quotes(round_id)
        ON DELETE CASCADE
);

CREATE TABLE client_tree_txids (
    -- txid is the 32-byte transaction hash (node.Input.Hash or computed from node).
    txid BLOB NOT NULL,

    -- round_id links to the round.
    round_id TEXT NOT NULL,

    -- client_key identifies which client's tree contains this txid.
    client_key BLOB NOT NULL,

    -- tree_level indicates depth in the tree (0 = root, increasing toward leaves).
    tree_level INTEGER NOT NULL,

    -- output_index is which parent output this transaction spends.
    -- Useful for identifying the branch path.
    output_index INTEGER NOT NULL,

    PRIMARY KEY (txid, round_id, client_key),
    FOREIGN KEY (round_id, client_key)
        REFERENCES round_client_trees(round_id, client_key)
        ON DELETE CASCADE
);

CREATE INDEX idx_boarding_addresses_creation_time
    ON boarding_addresses(creation_time DESC);

CREATE INDEX idx_boarding_addresses_last_confirmed
    ON boarding_addresses(last_confirmed_height DESC);

CREATE INDEX idx_boarding_intents_conf_height
    ON boarding_intents(conf_height DESC);

CREATE INDEX idx_boarding_intents_creation_time
    ON boarding_intents(creation_time DESC);

CREATE INDEX idx_boarding_intents_pk_script
    ON boarding_intents(pk_script);

CREATE INDEX idx_boarding_intents_status
    ON boarding_intents(status);

CREATE UNIQUE INDEX idx_boarding_sweep_inputs_active_outpoint
    ON boarding_sweep_inputs(outpoint_hash, outpoint_index)
    WHERE status IN ('pending', 'published');

CREATE INDEX idx_boarding_sweep_inputs_outpoint
    ON boarding_sweep_inputs(outpoint_hash, outpoint_index);

CREATE INDEX idx_boarding_sweep_inputs_status
    ON boarding_sweep_inputs(status);

CREATE INDEX idx_boarding_sweeps_status
    ON boarding_sweeps(status);

CREATE INDEX idx_client_ledger_chain_txid
    ON ledger_entries(chain_txid)
    WHERE chain_txid IS NOT NULL;

CREATE INDEX idx_client_ledger_created
    ON ledger_entries(created_at DESC);

CREATE INDEX idx_client_ledger_credit
    ON ledger_entries(credit_account);

CREATE INDEX idx_client_ledger_debit
    ON ledger_entries(debit_account);

CREATE INDEX idx_client_ledger_event_type
    ON ledger_entries(event_type);

CREATE UNIQUE INDEX idx_client_ledger_idempotent_key
    ON ledger_entries(
        idempotency_key, event_type, debit_account, credit_account
    )
    WHERE idempotency_key IS NOT NULL;

CREATE UNIQUE INDEX idx_client_ledger_idempotent_round
    ON ledger_entries(round_id, event_type, debit_account, credit_account)
    WHERE round_id IS NOT NULL;

CREATE UNIQUE INDEX idx_client_ledger_idempotent_session
    ON ledger_entries(session_id, event_type, debit_account, credit_account)
    WHERE session_id IS NOT NULL;

CREATE INDEX idx_client_ledger_round
    ON ledger_entries(round_id);

CREATE INDEX idx_client_round_agg_nonce_state_round_id
    ON client_round_agg_nonce_state(round_id);

CREATE INDEX idx_client_round_effects_due
    ON client_round_effects(status, next_attempt_at, created_at);

CREATE INDEX idx_client_round_effects_round
    ON client_round_effects(round_id, status, created_at);

CREATE INDEX idx_client_round_forfeit_request_state_round_id
    ON client_round_forfeit_request_state(round_id);

CREATE INDEX idx_client_round_forfeit_sig_state_round_id
    ON client_round_forfeit_sig_state(round_id);

CREATE INDEX idx_client_round_nonce_state_round_id
    ON client_round_nonce_state(round_id);

CREATE INDEX idx_client_round_partial_sig_state_round_id
    ON client_round_partial_sig_state(round_id);

CREATE INDEX idx_client_round_pending_quotes_created
    ON client_round_pending_quotes(creation_time, round_id);

CREATE INDEX idx_client_tree_txids_tree
    ON client_tree_txids(round_id, client_key, tree_level);

CREATE INDEX idx_client_tree_txids_txid
    ON client_tree_txids(txid);

CREATE INDEX idx_mailbox_egress_correlation
    ON mailbox_egress(correlation_id);

CREATE INDEX idx_mailbox_egress_due
    ON mailbox_egress(status, next_attempt_at, created_at);

CREATE INDEX idx_mailbox_egress_pair
    ON mailbox_egress(connector, local_mailbox_id, remote_mailbox_id, created_at);

CREATE INDEX idx_mailbox_ingress_remote
    ON mailbox_ingress_cursors(remote_mailbox_id);

CREATE INDEX idx_oor_client_effects_due
    ON oor_client_effects(status, next_attempt_at, created_at);

CREATE INDEX idx_oor_client_sessions_active
    ON oor_client_sessions(direction, updated_at)
    WHERE completed_at IS NULL;

CREATE INDEX idx_oor_package_checkpoints_session
    ON oor_package_checkpoints(session_id, checkpoint_index ASC);

CREATE INDEX idx_oor_packages_direction_updated
    ON oor_packages(direction, updated_at DESC);

CREATE INDEX idx_oor_vtxo_bindings_session
    ON oor_vtxo_bindings(session_id);

CREATE INDEX idx_round_boarding_intents_round_id
    ON round_boarding_intents(round_id);

CREATE INDEX idx_rounds_commitment_txid
    ON rounds(commitment_txid);

CREATE INDEX idx_rounds_creation_time
    ON rounds(creation_time DESC);

CREATE INDEX idx_rounds_status
    ON rounds(status);

CREATE INDEX idx_unroll_effects_due
    ON unroll_effects(status, next_attempt_at, created_at);

CREATE INDEX idx_unroll_jobs_state_updated
    ON unroll_jobs(state, updated_at DESC);

CREATE INDEX idx_unroll_tx_progress_status
    ON unroll_tx_progress(status, updated_at DESC);

CREATE INDEX idx_unroll_watches_status
    ON unroll_watches(status, role, updated_at DESC);

CREATE INDEX idx_utxo_log_block
    ON wallet_utxo_log(block_height);

CREATE INDEX idx_utxo_log_classification
    ON wallet_utxo_log(classified_as);

CREATE INDEX idx_utxo_log_outpoint
    ON wallet_utxo_log(outpoint_hash, outpoint_index);

CREATE UNIQUE INDEX idx_utxo_log_outpoint_event
    ON wallet_utxo_log(outpoint_hash, outpoint_index, event);

CREATE INDEX idx_vtxo_ancestry_paths_vtxo
    ON vtxo_ancestry_paths(vtxo_outpoint_hash, vtxo_outpoint_index);

CREATE INDEX idx_vtxos_creation_time
    ON vtxos(creation_time DESC);

CREATE INDEX idx_vtxos_round_id
    ON vtxos(round_id);

CREATE INDEX idx_vtxos_spent
    ON vtxos(spent);

CREATE INDEX idx_vtxos_status
    ON vtxos(status);

CREATE INDEX idx_wallet_effects_due
    ON wallet_effects(status, next_attempt_at, created_at);

CREATE TABLE ledger_entries (
    entry_id INTEGER PRIMARY KEY AUTOINCREMENT,

    debit_account TEXT NOT NULL
        REFERENCES accounts(account_id),

    credit_account TEXT NOT NULL
        REFERENCES accounts(account_id),

    -- amount_sat is the entry amount in satoshis.
    amount_sat BIGINT NOT NULL CHECK (amount_sat > 0),

    -- round_id optionally links this entry to a round
    -- (16-byte UUID).
    round_id BLOB,

    -- session_id optionally links this entry to an OOR session
    -- (32-byte identifier). Kept as a distinct column from
    -- round_id so 16-byte rounds and 32-byte sessions do not
    -- share a type-overloaded column.
    session_id BLOB,

    -- idempotency_key is an optional outpoint-derived dedup
    -- key used by events that carry neither a round_id nor an
    -- OOR session_id (e.g. unilateral exit legs keyed by the
    -- exited VTXO's outpoint). Together with the partial unique
    -- index idx_client_ledger_idempotent_key below, it makes
    -- replay-after-crash a silent no-op for multi-leg events
    -- that would otherwise double-book on at-least-once
    -- delivery.
    idempotency_key BLOB,

    -- event_type classifies the entry.
    event_type TEXT NOT NULL
        REFERENCES ledger_event_types(event_type),

    -- description is a human-readable note.
    description TEXT NOT NULL,

    -- created_at is the Unix timestamp.
    created_at BIGINT NOT NULL, chain_txid BLOB, chain_vout INTEGER, confirmation_height INTEGER,

    -- Debit and credit must target different accounts.
    CHECK (debit_account != credit_account)
);

CREATE TABLE ledger_event_types (
    event_type TEXT PRIMARY KEY
);

CREATE TABLE mailbox_egress (
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

CREATE TABLE mailbox_ingress_cursors (
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

CREATE TABLE oor_client_ark_artifacts (
    session_id BLOB    NOT NULL REFERENCES oor_client_sessions(session_id) ON DELETE CASCADE,
    phase      TEXT    NOT NULL CHECK (phase IN (
        'unsigned', 'ark_signed', 'accepted', 'finalized_context'
    )),
    ark_psbt   BLOB    NOT NULL,
    created_at BIGINT  NOT NULL,
    updated_at BIGINT  NOT NULL,
    PRIMARY KEY (session_id, phase)
);

CREATE TABLE oor_client_checkpoints (
    session_id       BLOB    NOT NULL REFERENCES oor_client_sessions(session_id) ON DELETE CASCADE,
    checkpoint_index INTEGER NOT NULL,
    phase            TEXT    NOT NULL CHECK (phase IN ('unsigned', 'cosigned', 'finalized')),
    checkpoint_psbt  BLOB    NOT NULL,
    created_at       BIGINT  NOT NULL,
    updated_at       BIGINT  NOT NULL,
    PRIMARY KEY (session_id, checkpoint_index, phase)
);

CREATE TABLE oor_client_effects (
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

CREATE TABLE oor_client_incoming_hints (
    session_id          BLOB   NOT NULL REFERENCES oor_client_sessions(session_id) ON DELETE CASCADE,
    recipient_pk_script BLOB   NOT NULL,
    recipient_event_id  BIGINT NOT NULL,
    created_at          BIGINT NOT NULL,
    updated_at          BIGINT NOT NULL,
    UNIQUE (recipient_pk_script, recipient_event_id)
);

CREATE TABLE oor_client_incoming_metadata (
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

CREATE TABLE oor_client_inputs (
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

CREATE TABLE oor_client_recipients (
    session_id           BLOB    NOT NULL REFERENCES oor_client_sessions(session_id) ON DELETE CASCADE,
    output_index         INTEGER NOT NULL,
    pk_script            BLOB    NOT NULL,
    value_sat            BIGINT  NOT NULL,
    vtxo_policy_template BLOB,
    PRIMARY KEY (session_id, output_index)
);

CREATE TABLE oor_client_sessions (
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

CREATE TABLE oor_package_checkpoints (
    -- session_id references the owning OOR package row.
    session_id BLOB NOT NULL,

    -- checkpoint_index is the zero-based order inside the package.
    checkpoint_index INTEGER NOT NULL CHECK (checkpoint_index >= 0),

    -- checkpoint_psbt stores one serialized finalized checkpoint PSBT.
    checkpoint_psbt BLOB NOT NULL,

    -- created_at is the unix timestamp when this index row was inserted.
    created_at BIGINT NOT NULL,

    -- Primary key keeps one checkpoint row per package index.
    PRIMARY KEY (session_id, checkpoint_index),

    -- Session foreign key keeps checkpoint rows tied to package lifecycle.
    FOREIGN KEY (session_id) REFERENCES oor_packages(session_id)
        ON DELETE CASCADE
);

CREATE TABLE oor_package_directions (
    -- direction is the persisted package direction code.
    direction INTEGER PRIMARY KEY NOT NULL,

    -- name is the stable string representation of the direction code.
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE oor_packages (
    -- session_id is the stable OOR session identifier (Ark txid bytes).
    session_id BLOB PRIMARY KEY NOT NULL,

    -- direction encodes local package direction:
    --   0 = incoming (received by this client)
    --   1 = outgoing (sent by this client)
    direction INTEGER NOT NULL,

    -- ark_psbt is the canonical Ark transaction package.
    ark_psbt BLOB NOT NULL,

    -- created_at is the unix timestamp when the row was first written.
    created_at BIGINT NOT NULL,

    -- updated_at is the unix timestamp of the last row update.
    updated_at BIGINT NOT NULL,

    -- Direction enum foreign key.
    FOREIGN KEY (direction) REFERENCES oor_package_directions(direction)
);

CREATE TABLE oor_recipient_cursors (
    -- recipient_pk_script is the tracked recipient script key.
    recipient_pk_script BLOB PRIMARY KEY NOT NULL,

    -- last_event_id is the last successfully processed event ID.
    last_event_id BIGINT NOT NULL,

    -- updated_at is the unix timestamp of the last cursor update.
    updated_at BIGINT NOT NULL,

    -- last_session_id is the last processed session for debugging.
    last_session_id BLOB
);

CREATE TABLE oor_vtxo_binding_link_kinds (
    -- link_kind is the persisted relation code.
    link_kind INTEGER PRIMARY KEY NOT NULL,

    -- name is the stable string representation of the relation code.
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE oor_vtxo_bindings (
    -- outpoint identifies the local VTXO outpoint linked to this package.
    outpoint_hash BLOB NOT NULL,

    -- outpoint_index is the output index of the local outpoint.
    outpoint_index INTEGER NOT NULL CHECK (outpoint_index >= 0),

    -- session_id references the OOR package linked to this outpoint.
    session_id BLOB NOT NULL,

    -- output_index identifies the package output index (incoming) or
    -- enumerated input index (outgoing consumed input).
    output_index INTEGER NOT NULL CHECK (output_index >= 0),

    -- link_kind encodes outpoint relation to package:
    --   0 = created_output (outpoint created by Ark package)
    --   1 = consumed_input (outpoint consumed by outgoing package)
    link_kind INTEGER NOT NULL,

    -- recipient script and amount are intentionally not duplicated here.
    -- They are derived from the referenced vtxos row via outpoint joins.

    -- created_at is the unix timestamp when the binding was created.
    created_at BIGINT NOT NULL,

    -- updated_at is the unix timestamp of the last binding update.
    updated_at BIGINT NOT NULL,

    -- Primary key allows both created-output and consumed-input bindings
    -- to coexist for the same local outpoint.
    PRIMARY KEY (outpoint_hash, outpoint_index, link_kind),

    -- Unique key prevents duplicate relation rows for one session member.
    UNIQUE (session_id, output_index, link_kind),

    -- Session foreign key keeps bindings tied to package lifecycle.
    FOREIGN KEY (session_id) REFERENCES oor_packages(session_id)
        ON DELETE CASCADE,

    -- Link-kind enum foreign key.
    FOREIGN KEY (link_kind) REFERENCES oor_vtxo_binding_link_kinds(link_kind),

    -- Outpoint foreign key enforces that bindings only reference local
    -- VTXOs known to the round/vtxo persistence tables.
    FOREIGN KEY (outpoint_hash, outpoint_index) REFERENCES vtxos(
        outpoint_hash, outpoint_index
    )
);

CREATE TABLE owned_receive_script_sources (
    -- source is the persisted source code.
    source INTEGER PRIMARY KEY NOT NULL,

    -- name is the stable string representation of the source code.
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE owned_receive_scripts (
    -- pk_script is the owned receive script primary key.
    pk_script BLOB PRIMARY KEY NOT NULL,

    -- client_key_family is the wallet key family for this script.
    client_key_family BIGINT NOT NULL,

    -- client_key_index is the wallet key index for this script.
    client_key_index BIGINT NOT NULL,

    -- client_pubkey is the client key used in the checkpoint taptree.
    client_pubkey BLOB NOT NULL,

    -- operator_pubkey is the operator key used in the checkpoint taptree.
    operator_pubkey BLOB NOT NULL,

    -- exit_delay is the CSV delay used in the timeout branch.
    exit_delay BIGINT NOT NULL,

    -- source labels how this script was discovered/registered:
    --   0 = wallet
    --   1 = rpc
    --   2 = sync
    source INTEGER NOT NULL CHECK (source IN (0, 1, 2)),

    -- created_at is the unix timestamp when this script was registered.
    created_at BIGINT NOT NULL,

    -- last_used_at is an optional unix timestamp of latest usage.
    last_used_at BIGINT,

    -- Source enum foreign key.
    FOREIGN KEY (source) REFERENCES owned_receive_script_sources(source)
);

CREATE TABLE pending_board_requests (
    outpoint_hash BLOB NOT NULL,
    outpoint_index INTEGER NOT NULL,

    target_vtxo_count INTEGER NOT NULL DEFAULT 0,

    requested_at_unix BIGINT NOT NULL,

    PRIMARY KEY (outpoint_hash, outpoint_index),

    CHECK (target_vtxo_count >= 0),
    CHECK (requested_at_unix > 0)
);

CREATE TABLE pending_board_vtxo_requests (
    outpoint_hash BLOB NOT NULL,
    outpoint_index INTEGER NOT NULL,
    request_index INTEGER NOT NULL,

    amount BIGINT NOT NULL,
    is_change BOOLEAN NOT NULL,
    pk_script BLOB NOT NULL,
    expiry INTEGER NOT NULL,
    policy_template BLOB NOT NULL,

    client_pubkey BLOB NOT NULL,
    operator_pubkey BLOB NOT NULL,

    owner_key_family INTEGER NOT NULL DEFAULT -1,
    owner_key_index INTEGER NOT NULL DEFAULT -1,

    signing_key_family INTEGER NOT NULL,
    signing_key_index INTEGER NOT NULL,
    signing_pubkey BLOB NOT NULL,

    origin INTEGER NOT NULL,

    PRIMARY KEY (outpoint_hash, outpoint_index, request_index),
    FOREIGN KEY (outpoint_hash, outpoint_index)
        REFERENCES pending_board_requests(outpoint_hash, outpoint_index)
        ON DELETE CASCADE
);

CREATE TABLE round_boarding_intents (
    -- round_id links to the parent round.
    round_id TEXT NOT NULL,

    -- outpoint_hash and outpoint_index reference the boarding_intents table.
    -- This is also the BoardingRequest.Outpoint field.
    outpoint_hash BLOB NOT NULL,
    outpoint_index INTEGER NOT NULL,

    -- BoardingRequest.ClientKey - 33-byte compressed public key.
    client_key BLOB NOT NULL,

    -- BoardingRequest.OperatorKey - 33-byte compressed public key.
    operator_key BLOB NOT NULL,

    -- BoardingRequest.ExitDelay - CSV delay for unilateral timeout.
    exit_delay INTEGER NOT NULL,

    -- policy_template is the semantic arkscript policy for this boarding
    -- request. This is the authoritative representation; the decomposed key
    -- and delay columns remain as denormalized standard-policy helpers.
    policy_template BLOB,

    -- BoardingRequest.TxProof - TLV-encoded proof.TxProof.
    -- NULL if the Option is None (server verifies via chain source).
    tx_proof BLOB,

    -- input_index is the position of this boarding input in the commitment tx.
    -- NULL until commitment tx is built.
    input_index INTEGER,

    -- input_signature is the client's schnorr signature for this boarding input.
    -- NULL until signatures are generated.
    input_signature BLOB,

    PRIMARY KEY (round_id, outpoint_hash, outpoint_index),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE,
    FOREIGN KEY (outpoint_hash, outpoint_index)
        REFERENCES boarding_intents(outpoint_hash, outpoint_index)
);

CREATE TABLE round_client_trees (
    -- round_id links to the parent round.
    round_id TEXT NOT NULL,

    -- client_key is the 33-byte compressed public key identifying the client.
    client_key BLOB NOT NULL,

    -- tree_data is the TLV-encoded tree.Tree.
    tree_data BLOB NOT NULL,

    PRIMARY KEY (round_id, client_key),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE
);

CREATE TABLE round_statuses (
    id BIGINT PRIMARY KEY,
    status_name TEXT UNIQUE NOT NULL
);

CREATE TABLE round_vtxo_requests (
    -- round_id links to the parent round.
    round_id TEXT NOT NULL,

    -- request_index preserves request order.
    request_index INTEGER NOT NULL,

    -- VTXORequest.Amount - value in satoshis.
    amount BIGINT NOT NULL,

    -- VTXORequest.PkScript - output script for the VTXO.
    pk_script BLOB NOT NULL,

    -- VTXORequest.Expiry - CSV delay for unilateral exit.
    expiry INTEGER NOT NULL,

    -- policy_template is the semantic arkscript policy for this requested
    -- output. This is the authoritative representation; the decomposed key
    -- and delay columns remain as denormalized standard-policy helpers.
    policy_template BLOB,

    -- VTXORequest.ClientKey - 33-byte compressed public key.
    client_pubkey BLOB NOT NULL,

    -- VTXORequest.OperatorKey - 33-byte compressed public key.
    operator_pubkey BLOB NOT NULL,

    -- VTXORequest.OwnerKey.KeyLocator.Family. A value of -1 means the
    -- request is foreign-owned and should not be persisted as local balance
    -- when the round confirms.
    owner_key_family INTEGER NOT NULL DEFAULT -1,

    -- VTXORequest.OwnerKey.KeyLocator.Index. A value of -1 means the request
    -- is foreign-owned and has no local owner descriptor.
    owner_key_index INTEGER NOT NULL DEFAULT -1,

    -- VTXORequest.SigningKey.KeyLocator.Family
    signing_key_family INTEGER NOT NULL,

    -- VTXORequest.SigningKey.KeyLocator.Index
    signing_key_index INTEGER NOT NULL,

    -- VTXORequest.SigningKey.PubKey - 33-byte compressed public key.
    signing_pubkey BLOB NOT NULL,

    PRIMARY KEY (round_id, request_index),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE
);

CREATE TABLE rounds (
    -- round_id is the unique identifier assigned by the server.
    round_id TEXT PRIMARY KEY NOT NULL,

    -- start_height is the block height when the round was created. Used as
    -- a HeightHint for confirmation registration when restoring from disk.
    start_height INTEGER NOT NULL DEFAULT 0,

    -- confirmation_height is the block height at which the commitment tx
    -- was confirmed. NULL until confirmed on-chain.
    confirmation_height INTEGER,

    -- confirmation_block_hash is the 32-byte hash of the block containing
    -- the commitment transaction. NULL until confirmed on-chain.
    confirmation_block_hash BLOB,

    -- commitment_tx is the serialized wire.MsgTx (binary).
    -- NULL until the server constructs the commitment transaction.
    commitment_tx BLOB,

    -- commitment_txid is the 32-byte hash of the commitment transaction.
    -- Indexed for efficient lookup by txid.
    commitment_txid BLOB,

    -- vtxt_tree is the TLV-encoded tree.Tree (Virtual Transaction Tree).
    -- NULL until the server provides the tree structure.
    vtxt_tree BLOB,

    -- status tracks round lifecycle combined with FSM state.
    -- At persistence time, this is always 'input_sig_sent' (point of no return).
    -- Transitions to 'confirmed', 'failed', or 'archived' as round progresses.
    status TEXT NOT NULL DEFAULT 'input_sig_sent',

    -- creation_time is the unix epoch timestamp when this round was created.
    creation_time BIGINT NOT NULL,

    -- last_update_time is the unix epoch timestamp of the last update.
    last_update_time BIGINT NOT NULL,

    FOREIGN KEY (status) REFERENCES round_statuses(status_name)
);

CREATE TABLE unroll_effects (
    id TEXT PRIMARY KEY,
    target_outpoint_hash BLOB NOT NULL,
    target_outpoint_index INTEGER NOT NULL CHECK (
        target_outpoint_index >= 0
    ),
    effect_type TEXT NOT NULL CHECK (effect_type IN (
        'subscribe_blocks',
        'watch_target_spend',
        'ensure_tx_confirmed',
        'watch_deferred_checkpoint',
        'build_sweep',
        'ensure_sweep_confirmed',
        'notify_registry'
    )),
    txid BLOB,
    status TEXT NOT NULL CHECK (status IN (
        'pending',
        'claimed',
        'done',
        'dead'
    )),
    idempotency_key TEXT NOT NULL UNIQUE,
    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 10,
    next_attempt_at BIGINT NOT NULL,
    claim_owner TEXT,
    claim_token TEXT,
    claim_until BIGINT,
    last_error TEXT,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    done_at BIGINT,

    FOREIGN KEY (target_outpoint_hash, target_outpoint_index)
        REFERENCES unroll_jobs(target_outpoint_hash, target_outpoint_index)
        ON DELETE CASCADE
);

CREATE TABLE unroll_jobs (
    -- target_outpoint_hash identifies the target transaction.
    target_outpoint_hash BLOB NOT NULL,

    -- target_outpoint_index identifies the target output index.
    target_outpoint_index INTEGER NOT NULL CHECK (
        target_outpoint_index >= 0
    ),

    -- state is the visible unroll FSM phase.
    state TEXT NOT NULL CHECK (state IN (
        'pending',
        'materializing',
        'csv_pending',
        'sweep_broadcast',
        'sweep_confirmation',
        'completed',
        'failed'
    )),

    -- trigger identifies what started the job.
    trigger TEXT NOT NULL CHECK (trigger IN (
        'manual',
        'critical_expiry',
        'restart',
        'fraud_spend'
    )),

    -- best_height is the latest chain height observed by this job.
    best_height INTEGER NOT NULL,

    -- target_confirm_height records the target confirmation height once known.
    target_confirm_height INTEGER,

    -- planner_state is the encoded unroll planner graph cursor.
    planner_state BLOB NOT NULL,

    -- deferred_checkpoints records fraud-triggered checkpoint deferrals.
    deferred_checkpoints BLOB,

    -- sweep_tx stores the exact final sweep transaction bytes after build.
    sweep_tx BLOB,

    -- sweep_txid is the 32-byte txid of the final sweep transaction once known.
    sweep_txid BLOB,

    -- sweep_confirm_height records the sweep confirmation height when known.
    sweep_confirm_height INTEGER,

    -- sweep_attempts counts sweep build/broadcast attempts.
    sweep_attempts INTEGER NOT NULL DEFAULT 0,

    -- fail_reason stores the terminal failure when present.
    fail_reason TEXT,

    -- created_at is the unix timestamp when the row was first written.
    created_at BIGINT NOT NULL,

    -- updated_at is the unix timestamp of the latest row update.
    updated_at BIGINT NOT NULL,

    PRIMARY KEY (target_outpoint_hash, target_outpoint_index)
);

CREATE TABLE unroll_tx_progress (
    target_outpoint_hash BLOB NOT NULL,
    target_outpoint_index INTEGER NOT NULL CHECK (
        target_outpoint_index >= 0
    ),
    txid BLOB NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('proof', 'deferred_checkpoint', 'sweep')),
    status TEXT NOT NULL CHECK (status IN (
        'ready',
        'in_flight',
        'confirmed',
        'failed'
    )),
    tx_bytes BLOB,
    confirm_height INTEGER,
    last_error TEXT,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,

    PRIMARY KEY (
        target_outpoint_hash, target_outpoint_index, txid, role
    ),
    FOREIGN KEY (target_outpoint_hash, target_outpoint_index)
        REFERENCES unroll_jobs(target_outpoint_hash, target_outpoint_index)
        ON DELETE CASCADE
);

CREATE TABLE unroll_watches (
    target_outpoint_hash BLOB NOT NULL,
    target_outpoint_index INTEGER NOT NULL CHECK (
        target_outpoint_index >= 0
    ),
    watch_id TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN (
        'block_epoch',
        'target_spend',
        'proof_tx',
        'deferred_checkpoint',
        'sweep'
    )),
    txid BLOB,
    spend_outpoint_hash BLOB,
    spend_outpoint_index INTEGER,
    status TEXT NOT NULL CHECK (status IN (
        'registered',
        'confirmed',
        'spent',
        'cancelled',
        'failed'
    )),
    height_hint INTEGER,
    confirmation_height INTEGER,
    last_error TEXT,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,

    PRIMARY KEY (target_outpoint_hash, target_outpoint_index, watch_id),
    FOREIGN KEY (target_outpoint_hash, target_outpoint_index)
        REFERENCES unroll_jobs(target_outpoint_hash, target_outpoint_index)
        ON DELETE CASCADE
);

CREATE TABLE utxo_classifications (
    classification TEXT PRIMARY KEY
);

CREATE TABLE utxo_events (
    event TEXT PRIMARY KEY
);

CREATE TABLE vtxo_ancestry_paths (
    -- vtxo_outpoint_hash and vtxo_outpoint_index identify the parent VTXO
    -- in the vtxos table.
    vtxo_outpoint_hash BLOB NOT NULL,
    vtxo_outpoint_index INTEGER NOT NULL,

    -- path_order is the deterministic ordinal of this fragment within
    -- the parent VTXO's ancestry, starting at 0. Persists the order
    -- chosen by the indexer (typically grouped by commitment_txid) so
    -- the unroller's broadcast plan is reproducible across restarts.
    path_order INTEGER NOT NULL,

    -- commitment_txid is the 32-byte commitment tx hash anchoring this
    -- fragment. Distinct rows for one VTXO must have distinct
    -- commitment_txids.
    commitment_txid BLOB NOT NULL,

    -- tree_path is the TLV-encoded extracted tree.Tree fragment from the
    -- batch root to the input VTXO leaf served by this fragment.
    tree_path BLOB NOT NULL,

    -- tree_depth is the depth of the served leaf within this fragment's
    -- tree. Worst-case unilateral-exit timing for the parent VTXO is
    -- max(tree_depth) across all fragments.
    tree_depth INTEGER NOT NULL,

    -- input_indices is a length-prefixed BE-uint32 list of Ark tx input
    -- indices (within the OOR Ark tx that produced the parent VTXO)
    -- that this fragment serves. Empty for round-direct VTXOs.
    --
    -- No SQL-level DEFAULT here: INSERT statements always pass an
    -- explicit value (empty length-prefixed slice for round-direct
    -- rows). A `DEFAULT X''` literal works on SQLite but is parsed by
    -- Postgres as a bit-string and rejected against the BYTEA column.
    input_indices BLOB NOT NULL,

    PRIMARY KEY (vtxo_outpoint_hash, vtxo_outpoint_index, path_order),
    FOREIGN KEY (vtxo_outpoint_hash, vtxo_outpoint_index)
        REFERENCES vtxos(outpoint_hash, outpoint_index)
        ON DELETE CASCADE,

    -- A VTXO must not carry two ancestry rows for the same commitment
    -- tx. Distinct fragments must anchor at distinct commitments
    -- (per the Ancestry contract); enforcing it at the schema level
    -- means a future caller bypassing BuildIncomingVTXODescriptor
    -- still cannot persist a malformed VTXO that would later trip a
    -- "conflicting proof node" deep inside addProofNode at unilateral
    -- exit time.
    UNIQUE (vtxo_outpoint_hash, vtxo_outpoint_index, commitment_txid),

    -- path_order must be a small non-negative ordinal. The active
    -- fragment-count cap (MaxAncestryFragments) is well under 64;
    -- this CHECK guards against a caller persisting a row at a
    -- nonsense ordinal (e.g. negative, or a uint32 round-trip from
    -- malformed wire data) without coupling the schema to the exact
    -- runtime cap.
    CHECK (path_order >= 0 AND path_order < 64)
);

CREATE TABLE vtxos (
    -- outpoint_hash and outpoint_index form the VTXO outpoint (primary key).
    outpoint_hash BLOB NOT NULL,
    outpoint_index INTEGER NOT NULL,

    -- round_id links to the round that created this VTXO.
    round_id TEXT NOT NULL,

    -- amount is the value in satoshis.
    amount BIGINT NOT NULL,

    -- pk_script is the output script for this VTXO.
    pk_script BLOB NOT NULL,

    -- expiry is the CSV delay in blocks.
    expiry INTEGER NOT NULL,

    -- policy_template is the semantic arkscript policy for this VTXO.
    -- This is the authoritative representation; the decomposed key and delay
    -- columns remain as denormalized standard-policy helpers.
    policy_template BLOB,

    -- client_key_family is the BIP32 key family.
    client_key_family INTEGER NOT NULL,

    -- client_key_index is the BIP32 key index.
    client_key_index INTEGER NOT NULL,

    -- client_pubkey is the 33-byte compressed client public key.
    client_pubkey BLOB NOT NULL,

    -- operator_pubkey is the 33-byte compressed operator public key.
    operator_pubkey BLOB NOT NULL,

    -- tree_path is the TLV-encoded extracted tree.Tree path.
    batch_expiry INTEGER NOT NULL,

    -- tree_depth is the depth of this VTXO in the VTXT (used for expiry
    -- calculation based on TreeDepthMultiplier). Zero for same reason.
    created_height INTEGER NOT NULL,

    -- commitment_txid is the 32-byte txid of the commitment transaction that
    -- anchors this VTXO's tree on-chain. Empty blob until the VTXO manager
    -- fills in the full metadata via ON CONFLICT DO UPDATE.
    commitment_txid BLOB NOT NULL,

    -- spent indicates if this VTXO has been used.
    spent BOOLEAN NOT NULL DEFAULT FALSE,

    -- status tracks VTXO lifecycle (vtxo.VTXOStatus enum):
    --   0 = Live (default)
    --   1 = PendingForfeit
    --   2 = Forfeiting
    --   3 = Forfeited
    --   4 = Spent
    --   5 = UnilateralExit
    --   6 = Failed
    --   7 = Spending
    status INTEGER NOT NULL DEFAULT 0,

    -- forfeit_round_id is the round in which this VTXO is being forfeited.
    -- NULL unless VTXO is in Forfeiting or Forfeited status.
    forfeit_round_id TEXT,

    -- forfeit_tx is the serialized wire.MsgTx (binary) of the forfeit tx.
    -- Persisted when entering Forfeiting state for crash recovery.
    forfeit_tx BLOB,

    -- forfeit_txid is the 32-byte hash of the forfeit transaction.
    -- Set when the forfeit is confirmed (transition to Forfeited state).
    forfeit_txid BLOB,

    -- replaced_by_hash is the outpoint hash of the replacement VTXO.
    replaced_by_hash BLOB,

    -- replaced_by_index is the outpoint index of the replacement VTXO.
    replaced_by_index INTEGER,

    -- creation_time is the unix epoch timestamp when this VTXO was created.
    creation_time BIGINT NOT NULL,

    -- last_update_time is the unix epoch timestamp when this VTXO was last
    -- modified, such as when it was marked as spent.
    last_update_time BIGINT NOT NULL, chain_depth INTEGER NOT NULL DEFAULT 0,

    PRIMARY KEY (outpoint_hash, outpoint_index),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id)
);

CREATE TABLE wallet_effects (
    id TEXT PRIMARY KEY,
    effect_type TEXT NOT NULL CHECK (effect_type IN (
        'record_ledger_sweep_fee',
        'record_ledger_utxo_created',
        'record_ledger_utxo_spent'
    )),
    status TEXT NOT NULL CHECK (status IN (
        'pending', 'claimed', 'done', 'dead'
    )),
    idempotency_key TEXT NOT NULL UNIQUE,

    outpoint_hash BLOB,
    outpoint_index INTEGER,
    txid BLOB,
    amount_sat BIGINT,
    fee_sat BIGINT,
    block_height INTEGER,
    classification TEXT,

    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 10,
    next_attempt_at BIGINT NOT NULL,
    claim_owner TEXT,
    claim_token TEXT,
    claim_until BIGINT,
    last_error TEXT,

    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    done_at BIGINT,

    CHECK (id <> ''),
    CHECK (idempotency_key <> ''),
    CHECK (attempts >= 0),
    CHECK (max_attempts > 0),
    CHECK (next_attempt_at > 0),
    CHECK (created_at > 0),
    CHECK (updated_at >= created_at)
);

CREATE TABLE wallet_utxo_log (
    entry_id INTEGER PRIMARY KEY AUTOINCREMENT,

    -- outpoint_hash is the transaction hash (32 bytes).
    outpoint_hash BLOB NOT NULL,

    -- outpoint_index is the output index.
    outpoint_index INTEGER NOT NULL,

    -- amount_sat is the UTXO value.
    amount_sat BIGINT NOT NULL,

    -- event is 'created' or 'spent'.
    event TEXT NOT NULL
        REFERENCES utxo_events(event),

    -- block_height is the block where this change occurred.
    block_height INTEGER NOT NULL,

    -- classified_as categorizes the UTXO event.
    classified_as TEXT NOT NULL
        REFERENCES utxo_classifications(classification),

    -- created_at is the Unix timestamp when this entry was
    -- recorded.
    created_at BIGINT NOT NULL
);

