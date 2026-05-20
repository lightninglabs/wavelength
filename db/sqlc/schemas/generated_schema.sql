CREATE TABLE account_types (
    account_type TEXT PRIMARY KEY
);

CREATE TABLE accounts (
    account_id TEXT PRIMARY KEY,
    account_name TEXT NOT NULL,
    account_type TEXT NOT NULL
        REFERENCES account_types(account_type)
);

CREATE TABLE ask_results (
    -- promise_id links to the original Ask message.
    promise_id TEXT PRIMARY KEY,

    -- result_blob contains the TLV-encoded successful result.
    -- NULL if the request failed with an error.
    result_blob BLOB,

    -- error_text contains the error message if the request failed.
    -- NULL if the request succeeded.
    error_text TEXT,

    -- created_at is the unix timestamp when the result was persisted.
    created_at BIGINT NOT NULL,

    -- expires_at is the unix timestamp after which this result can be garbage
    -- collected. Callers should retrieve results before expiry.
    expires_at BIGINT NOT NULL
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

CREATE TABLE dead_letters (
    -- id is the original message ID.
    id TEXT PRIMARY KEY,

    -- source indicates where the message originated: 'mailbox' or 'outbox'.
    source TEXT NOT NULL,

    -- actor_id identifies the target actor (for mailbox) or source (for outbox).
    actor_id TEXT NOT NULL,

    -- message_type is the type name for the failed message.
    message_type TEXT NOT NULL,

    -- payload contains the original TLV-encoded message data.
    payload BLOB NOT NULL,

    -- failure_reason describes why the message was dead-lettered.
    failure_reason TEXT NOT NULL,

    -- attempts is the number of delivery attempts before dead-lettering.
    attempts INTEGER NOT NULL,

    -- created_at is the unix timestamp when the message was dead-lettered.
    created_at BIGINT NOT NULL
);

CREATE TABLE fsm_checkpoints (
    -- actor_id identifies the actor whose FSM state is checkpointed.
    actor_id TEXT PRIMARY KEY,

    -- state_type is the name of the current FSM state for quick lookup.
    state_type TEXT NOT NULL,

    -- state_data contains the TLV-encoded state snapshot.
    state_data BLOB NOT NULL,

    -- version is a monotonic counter incremented on each checkpoint.
    -- Used for conflict detection and debugging.
    version INTEGER NOT NULL DEFAULT 0,

    -- updated_at is the unix timestamp of the last checkpoint.
    updated_at BIGINT NOT NULL
);

CREATE INDEX idx_ask_results_expires
    ON ask_results(expires_at);

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

CREATE INDEX idx_client_tree_txids_tree
    ON client_tree_txids(round_id, client_key, tree_level);

CREATE INDEX idx_client_tree_txids_txid
    ON client_tree_txids(txid);

CREATE INDEX idx_dead_letters_actor
    ON dead_letters(actor_id, created_at DESC);

CREATE INDEX idx_dead_letters_source
    ON dead_letters(source, created_at DESC);

CREATE INDEX idx_mailbox_messages_available
    ON mailbox_messages(mailbox_id, priority DESC, available_at ASC, created_at ASC);

CREATE INDEX idx_mailbox_messages_correlation
    ON mailbox_messages(mailbox_id, correlation_key, id)
    WHERE correlation_key IS NOT NULL;

CREATE INDEX idx_mailbox_messages_lease
    ON mailbox_messages(lease_until)
    WHERE lease_until IS NOT NULL;

CREATE INDEX idx_mailbox_messages_promise
    ON mailbox_messages(promise_id)
    WHERE promise_id IS NOT NULL;

CREATE INDEX idx_oor_package_checkpoints_session
    ON oor_package_checkpoints(session_id, checkpoint_index ASC);

CREATE INDEX idx_oor_packages_direction_updated
    ON oor_packages(direction, updated_at DESC);

CREATE INDEX idx_oor_vtxo_bindings_session
    ON oor_vtxo_bindings(session_id);

CREATE INDEX idx_outbox_messages_domain_key
    ON outbox_messages(domain_key)
    WHERE domain_key IS NOT NULL;

CREATE INDEX idx_outbox_messages_pending
    ON outbox_messages(status, created_at)
    WHERE status = 'pending';

CREATE INDEX idx_processed_messages_expires
    ON processed_messages(expires_at);

CREATE INDEX idx_round_boarding_intents_round_id
    ON round_boarding_intents(round_id);

CREATE INDEX idx_rounds_commitment_txid
    ON rounds(commitment_txid);

CREATE INDEX idx_rounds_creation_time
    ON rounds(creation_time DESC);

CREATE INDEX idx_rounds_status
    ON rounds(status);

CREATE INDEX idx_unilateral_exit_jobs_status_updated
    ON unilateral_exit_jobs(status, updated_at DESC);

CREATE INDEX idx_utxo_log_block
    ON wallet_utxo_log(block_height);

CREATE INDEX idx_utxo_log_classification
    ON wallet_utxo_log(classified_as);

CREATE INDEX idx_utxo_log_outpoint
    ON wallet_utxo_log(outpoint_hash, outpoint_index);

CREATE UNIQUE INDEX idx_utxo_log_outpoint_event
    ON wallet_utxo_log(outpoint_hash, outpoint_index, event);

CREATE INDEX idx_vhtlc_recovery_jobs_state_updated
    ON vhtlc_recovery_jobs(state, updated_at DESC);

CREATE INDEX idx_vhtlc_recovery_jobs_swap_action
    ON vhtlc_recovery_jobs(swap_id, action);

CREATE INDEX idx_vhtlc_recovery_jobs_unroll_target
    ON vhtlc_recovery_jobs(
        unroll_target_outpoint_hash,
        unroll_target_outpoint_index
    )
    WHERE unroll_target_outpoint_hash IS NOT NULL;

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

CREATE TABLE mailbox_messages (
    -- id is a UUIDv7 providing time-ordering and uniqueness.
    id TEXT PRIMARY KEY,

    -- mailbox_id identifies the target actor's mailbox.
    mailbox_id TEXT NOT NULL,

    -- message_type is the type name for deserialization dispatch.
    message_type TEXT NOT NULL,

    -- payload contains the TLV-encoded message data.
    payload BLOB NOT NULL,

    -- promise_id is set for Ask messages to track the response.
    -- NULL for Tell (fire-and-forget) messages.
    promise_id TEXT,

    -- callback_actor_id is set for DurableAsk messages to route the response.
    -- The response will be delivered to this actor's mailbox via outbox.
    -- NULL for regular Ask/Tell messages.
    callback_actor_id TEXT,

    -- correlation_id links DurableAsk requests to their responses.
    -- The response message will include this ID for matching.
    -- NULL for regular Ask/Tell messages.
    correlation_id TEXT,

    -- priority determines processing order (higher = more important).
    -- Used for restart messages which need front-of-queue processing.
    priority INTEGER NOT NULL DEFAULT 0,

    -- Lease management fields.
    -- lease_token is an opaque token that must match for Ack/Nack to succeed.
    -- This prevents stale acks from a previous lease holder after crash.
    lease_token TEXT,

    -- lease_until is the unix timestamp when the lease expires.
    -- After expiry, the message becomes available for redelivery.
    lease_until BIGINT,

    -- Delivery tracking fields.
    -- available_at is the unix timestamp when the message becomes available.
    -- Used for scheduling initial delivery and retry delays after Nack.
    available_at BIGINT NOT NULL,

    -- attempts tracks how many times delivery has been attempted.
    attempts INTEGER NOT NULL DEFAULT 0,

    -- max_attempts is the maximum delivery attempts before dead-lettering.
    max_attempts INTEGER NOT NULL DEFAULT 10,

    -- created_at is the unix timestamp when the message was enqueued.
    created_at BIGINT NOT NULL
, correlation_key TEXT);

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

CREATE TABLE outbox_messages (
    -- id is a UUIDv7 providing time-ordering and uniqueness.
    id TEXT PRIMARY KEY,

    -- source_actor_id identifies the actor that created this message.
    source_actor_id TEXT NOT NULL,

    -- target_actor_id identifies the destination actor's mailbox.
    target_actor_id TEXT NOT NULL,

    -- message_type is the type name for deserialization dispatch.
    message_type TEXT NOT NULL,

    -- payload contains the TLV-encoded message data.
    payload BLOB NOT NULL,

    -- domain_key is an optional natural idempotency key.
    -- For example: "round:abc123:phase:nonces" ensures the same round/phase
    -- combination is only processed once by the receiver.
    domain_key TEXT,

    -- version is a monotonic counter for ordering within a domain.
    -- Higher versions supersede lower versions for the same domain_key.
    version INTEGER NOT NULL DEFAULT 0,

    -- status tracks the delivery lifecycle.
    -- Values: 'pending', 'completed', 'dead_letter'
    status TEXT NOT NULL DEFAULT 'pending',

    -- delivery_attempts tracks how many times delivery was attempted.
    delivery_attempts INTEGER NOT NULL DEFAULT 0,

    -- Claim management fields for concurrent publisher safety.
    -- claim_token is an opaque token set by ClaimOutboxBatch. CompleteOutbox
    -- and FailOutbox must present a matching token to mutate the message,
    -- preventing a slow publisher from completing a message that was already
    -- reclaimed by another publisher after lease expiry.
    claim_token TEXT,

    -- claimed_until is the unix timestamp when the current claim expires.
    -- After expiry, the message becomes available for reclaim.
    claimed_until BIGINT,

    -- created_at is the unix timestamp when the message was enqueued.
    created_at BIGINT NOT NULL,

    -- completed_at is the unix timestamp when delivery completed (or failed).
    completed_at BIGINT
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

CREATE TABLE processed_messages (
    -- id is the message ID that was processed.
    id TEXT PRIMARY KEY,

    -- actor_id identifies which actor processed this message.
    actor_id TEXT NOT NULL,

    -- processed_at is the unix timestamp when processing completed.
    processed_at BIGINT NOT NULL,

    -- expires_at is the unix timestamp after which this entry can be deleted.
    -- Should exceed the maximum possible redelivery window.
    expires_at BIGINT NOT NULL
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

CREATE TABLE unilateral_exit_jobs (
    -- target_outpoint_hash identifies the target transaction.
    target_outpoint_hash BLOB NOT NULL,

    -- target_outpoint_index identifies the target output index.
    target_outpoint_index INTEGER NOT NULL CHECK (
        target_outpoint_index >= 0
    ),

    -- actor_id is the durable actor mailbox id for this target job.
    actor_id TEXT NOT NULL,

    -- status is the control-plane job status:
    --   0 = pending
    --   1 = materializing
    --   2 = csv_pending
    --   3 = sweeping (sweep broadcast, awaiting confirmation)
    --   4 = completed
    --   5 = failed
    --   6 = sweep_broadcasting (sweep built, not yet submitted)
    status INTEGER NOT NULL,

    -- trigger identifies what started the job:
    --   0 = manual
    --   1 = critical_expiry
    --   2 = restart
    --   3 = fraud_spend
    trigger INTEGER NOT NULL,

    -- last_error stores the latest terminal or diagnostic error string.
    last_error TEXT,

    -- sweep_txid is the 32-byte txid of the final sweep transaction.
    -- NULL until the sweep is broadcast.
    sweep_txid BLOB,

    -- created_at is the unix timestamp when the row was first written.
    created_at BIGINT NOT NULL,

    -- updated_at is the unix timestamp of the latest row update.
    updated_at BIGINT NOT NULL,

    PRIMARY KEY (target_outpoint_hash, target_outpoint_index)
);

CREATE TABLE utxo_classifications (
    classification TEXT PRIMARY KEY
);

CREATE TABLE utxo_events (
    event TEXT PRIMARY KEY
);

CREATE TABLE vhtlc_recovery_jobs (
    -- id is the daemon-owned recovery identifier returned to callers and used
    -- in logs. It is distinct from request_id so retries can be idempotent
    -- without forcing the caller to pick the durable row id.
    id TEXT PRIMARY KEY,

    -- request_id is the caller-owned idempotency key. Repeating a request with
    -- the same request_id returns the existing row only when the durable
    -- parameters match.
    request_id TEXT NOT NULL UNIQUE,

    -- swap_id links the recovery action back to the swap's durable state. The
    -- swap table remains the source of truth for swap lifecycle and preimage
    -- material.
    swap_id BLOB NOT NULL,

    -- direction records which side of the swap owns this recovery action. It
    -- is intentionally denormalized for logs and SQL/Grafana queries.
    direction TEXT NOT NULL CHECK (
        direction IN ('pay', 'receive', 'server_in', 'server_out')
    ),

    -- action selects the unilateral vHTLC leaf this job is allowed to spend.
    -- Cooperative refund with the receiver is not a recovery action; it stays
    -- on the cooperative OOR path.
    action TEXT NOT NULL CHECK (
        action IN ('claim', 'refund_without_receiver')
    ),

    -- state is the recovery FSM state. Terminal states are completed,
    -- cancelled, and failed. waiting_for_target and building_exit_spend are
    -- written by the execution-layer PR; this schema accepts them now so the
    -- later worker can restart from every pipeline boundary. cancelled means
    -- cooperative resolution won before recovery spent on-chain; failed means
    -- recovery needs operator attention.
    state TEXT NOT NULL CHECK (state IN (
        'armed',
        'unroll_started',
        'waiting_for_target',
        'waiting_for_csv',
        'building_exit_spend',
        'exit_spend_built',
        'submitting_exit_spend',
        'exit_spend_pending_confirmation',
        'completed',
        'cancelled',
        'failed'
    )),

    -- vtxo_* identifies the vHTLC VTXO that the unroll subsystem must
    -- materialize on-chain before this recovery can build its final exit
    -- spend.
    vtxo_txid BLOB NOT NULL,
    vtxo_vout INTEGER NOT NULL CHECK (vtxo_vout >= 0),
    vtxo_amount_sat BIGINT NOT NULL CHECK (vtxo_amount_sat > 0),

    -- *_pubkey columns are the vHTLC policy participants needed to reconstruct
    -- and validate the output script. They are public keys, not private
    -- signing material.
    sender_pubkey BLOB NOT NULL,
    receiver_pubkey BLOB NOT NULL,
    server_pubkey BLOB NOT NULL,

    -- Timelock and CSV parameters reconstruct the exact vHTLC policy leaves.
    -- refund_locktime is stored as SQLite INTEGER/sqlc int32 even though
    -- Bitcoin locktimes are unsigned; policy construction validates it before
    -- converting to wire-format locktime values. The CSV parameters are copied
    -- into the recovery row so the job can restart without depending on
    -- in-memory swap FSM state.
    refund_locktime INTEGER NOT NULL CHECK (refund_locktime > 0),
    unilateral_claim_delay INTEGER NOT NULL CHECK (
        unilateral_claim_delay > 0
    ),
    unilateral_refund_delay INTEGER NOT NULL CHECK (
        unilateral_refund_delay > 0
    ),
    unilateral_refund_without_receiver_delay INTEGER NOT NULL CHECK (
        unilateral_refund_without_receiver_delay > 0
    ),

    -- preimage_hash is safe to persist and log. It is the stable lookup key
    -- for claim-preimage material.
    preimage_hash BLOB NOT NULL,

    -- claim_preimage is nullable secret witness material. It is populated only
    -- for cross-process claim recovery where the daemon cannot call an
    -- in-process swap preimage resolver. The value must never be logged.
    claim_preimage BLOB,

    -- signer_key_* identifies the wallet key that signs the exit spend. It is
    -- a key locator, not a private key.
    signer_key_family INTEGER NOT NULL,
    signer_key_index INTEGER NOT NULL,

    -- destination_script is the wallet-controlled output script that receives
    -- recovered funds once the vHTLC exit spend confirms.
    destination_script BLOB NOT NULL,

    -- max_fee_rate_sat_per_kw caps the fee rate, in sat/kw, that the recovery
    -- worker may pay for the final exit spend. If the estimator exceeds this
    -- cap, recovery pauses/fails according to the worker policy rather than
    -- silently overpaying.
    max_fee_rate_sat_per_kw INTEGER NOT NULL CHECK (
        max_fee_rate_sat_per_kw > 0
    ),

    -- unroll_target_outpoint_* records the materialized on-chain output once
    -- unroll has produced it. Until then these columns remain NULL and the
    -- recovery job watches the unroll job for progress.
    unroll_target_outpoint_hash BLOB,
    unroll_target_outpoint_index INTEGER CHECK (
        unroll_target_outpoint_index IS NULL OR
        unroll_target_outpoint_index >= 0
    ),

    -- exit_policy_kind is the unroll policy kind registered by this recovery
    -- action. The CHECK is local to vHTLC recovery because this table owns only
    -- the vHTLC policy variants; generic unroll policy extensibility lives on
    -- unroll_jobs.
    exit_policy_kind TEXT NOT NULL CHECK (exit_policy_kind IN (
        'vhtlc_claim',
        'vhtlc_refund_without_receiver'
    )),

    -- exit_tx is the exact signed exit transaction persisted before broadcast.
    -- exit_txid is denormalized for log/search convenience. cooperative_txid
    -- records the transaction that made recovery unnecessary when the job is
    -- cancelled by a cooperative resolution.
    exit_tx BLOB,
    exit_txid BLOB,
    cooperative_txid BLOB,

    -- last_error is the latest retry or terminal failure detail. cancel_reason
    -- records why recovery was cancelled, usually because cooperative
    -- settlement won the race.
    last_error TEXT,
    cancel_reason TEXT,

    -- *_at columns are unix timestamps used for restart ordering, operator
    -- runbooks, and SQL/Grafana observability without requiring a separate
    -- metrics surface in v1.
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    armed_at BIGINT,
    escalated_at BIGINT,
    target_detected_at BIGINT,
    exit_tx_built_at BIGINT,
    exit_tx_broadcast_at BIGINT,
    terminal_at BIGINT,

    -- At most one claim and one refund-without-receiver recovery can exist for
    -- a swap. This also makes retries by swap/action safe when the caller lost
    -- the original request_id.
    UNIQUE(swap_id, action)
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

