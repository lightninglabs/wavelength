-- Round FSM persistence: rounds, their boarding intents and VTXO
-- requests, and the per-client tree extracts.

-- Enum-like table for round lifecycle states.
CREATE TABLE IF NOT EXISTS round_statuses (
    id BIGINT PRIMARY KEY,
    status_name TEXT UNIQUE NOT NULL
);

-- Populate the possible round statuses.
-- These combine lifecycle state with FSM state at the "point of no return".
INSERT INTO round_statuses (id, status_name) VALUES
    (0, 'input_sig_sent'),  -- Client sent input signatures, awaiting confirmation
    (1, 'confirmed'),       -- Commitment tx confirmed, VTXOs created
    (2, 'failed'),          -- Round failed, intents may need recovery
    (3, 'archived');        -- Round finalized and archived

-- Main rounds table.
-- Rounds coordinate boarding intents (and future: refreshes, offboards) into
-- a single commitment transaction. State data is stored relationally with
-- tree structures serialized as TLV blobs.
CREATE TABLE IF NOT EXISTS rounds (
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

    -- flow_version records the per-round flow version: the choreography
    -- rules under which this round was created. The operator stamps it and
    -- the client records the same value from the batch info. It never
    -- changes. The versions are zero-indexed, so the only understood value
    -- today is 0 (V1); a future, genuinely different round flow is added
    -- additively (V2 == 1, and so on). NOT NULL DEFAULT 0 keeps every row a
    -- valid V1 round.
    flow_version INTEGER NOT NULL DEFAULT 0,

    FOREIGN KEY (status) REFERENCES round_statuses(status_name)
);

-- Index on commitment_txid for LookupRoundByCommitmentTx.
CREATE INDEX IF NOT EXISTS idx_rounds_commitment_txid
    ON rounds(commitment_txid);

-- Index on status for ListActiveRounds.
CREATE INDEX IF NOT EXISTS idx_rounds_status
    ON rounds(status);

-- Index on creation_time for chronological queries.
CREATE INDEX IF NOT EXISTS idx_rounds_creation_time
    ON rounds(creation_time DESC);

-- Round boarding intents table.
-- Links boarding intents to rounds with round-specific data.
-- References the existing boarding_intents table.
-- BoardingRequest fields stored relationally (Outpoint is the FK columns).
CREATE TABLE IF NOT EXISTS round_boarding_intents (
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

-- Index on round_id for efficient lookup of intents by round.
CREATE INDEX IF NOT EXISTS idx_round_boarding_intents_round_id
    ON round_boarding_intents(round_id);

-- Round VTXO requests table.
-- Stores VTXO requests for the round.
CREATE TABLE IF NOT EXISTS round_vtxo_requests (
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

    -- owner_key_id references the internal_keys registry row for the local
    -- owner descriptor (the client_pubkey paired with its lnd KeyLocator).
    -- NULL means the request is foreign-owned: it has no local owner
    -- descriptor and must not be persisted as local balance when the round
    -- confirms. This replaces the old -1/-1 sentinel locator.
    owner_key_id BIGINT REFERENCES internal_keys(id),

    -- signing_key_id references the internal_keys registry row for the
    -- signing descriptor (signing_pubkey paired with its lnd KeyLocator).
    signing_key_id BIGINT REFERENCES internal_keys(id),

    PRIMARY KEY (round_id, request_index),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE
);

-- Client trees table.
-- Stores extracted tree paths for each client key in a round.
-- These are the pruned paths that contain only nodes relevant to each client.
CREATE TABLE IF NOT EXISTS round_client_trees (
    -- round_id links to the parent round.
    round_id TEXT NOT NULL,

    -- client_key is the 33-byte compressed public key identifying the client.
    client_key BLOB NOT NULL,

    -- tree_data is the TLV-encoded tree.Tree.
    tree_data BLOB NOT NULL,

    PRIMARY KEY (round_id, client_key),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE
);

-- Client tree txids table.
-- Associative table mapping transaction IDs to client trees for efficient lookup.
-- When the chain backend confirms a txid, we can quickly find which client tree
-- contains it without deserializing all tree blobs.
CREATE TABLE IF NOT EXISTS client_tree_txids (
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

-- Index on txid for fast lookup when chain confirms a transaction.
CREATE INDEX IF NOT EXISTS idx_client_tree_txids_txid
    ON client_tree_txids(txid);

-- Index for finding all txids in a specific client tree.
CREATE INDEX IF NOT EXISTS idx_client_tree_txids_tree
    ON client_tree_txids(round_id, client_key, tree_level);
