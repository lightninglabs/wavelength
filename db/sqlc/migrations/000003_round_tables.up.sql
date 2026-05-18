-- Round tables migration.
-- This migration creates tables for round FSM persistence and VTXO management.

-- Enum-like table for round lifecycle states.
CREATE TABLE IF NOT EXISTS round_statuses (
    id BIGINT PRIMARY KEY,
    status_name TEXT UNIQUE NOT NULL
);

-- Populate the possible round statuses.
-- These combine lifecycle state with FSM state at the "point of no return".
INSERT INTO round_statuses (id, status_name) VALUES
    (0, 'input_sig_sent'),    -- Client sent input signatures, awaiting confirmation
    (1, 'confirmed'),         -- Commitment tx confirmed, VTXOs created
    (2, 'failed'),            -- Round failed, intents may need recovery
    (3, 'archived'),          -- Round finalized and archived
    (4, 'nonces_generated'),  -- Client persisted MuSig2 nonces before send
    (5, 'nonces_aggregated'), -- Client persisted operator aggregate nonces
    (6, 'partial_sigs_sent'), -- Client persisted MuSig2 partial signatures
    (7, 'forfeit_sigs_collecting'); -- Awaiting VTXO actor forfeit sigs

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

-- Client round nonce state table.
-- Stores local MuSig2 nonce material before public nonces are sent to the
-- server. The secret nonce is required to recreate signing sessions after a
-- restart and must never be reused across distinct rounds.
CREATE TABLE IF NOT EXISTS client_round_nonce_state (
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

CREATE INDEX IF NOT EXISTS idx_client_round_nonce_state_round_id
    ON client_round_nonce_state(round_id);

-- Client round aggregate nonce state.
-- Stores the operator-aggregated MuSig2 public nonce for each transaction once
-- received. On restart the actor reconstructs local signing sessions from
-- client_round_nonce_state, registers these aggregate nonces, and regenerates
-- partial signatures without producing new local nonces.
CREATE TABLE IF NOT EXISTS client_round_agg_nonce_state (
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

CREATE INDEX IF NOT EXISTS idx_client_round_agg_nonce_state_round_id
    ON client_round_agg_nonce_state(round_id);

-- Client round partial signature state.
-- Stores generated MuSig2 partial signatures before they are sent to the
-- server. The signatures are deterministic given persisted local secret nonces
-- and operator aggregate nonces, but persisting them gives the send effect a
-- simple SQL fact table to replay after restart.
CREATE TABLE IF NOT EXISTS client_round_partial_sig_state (
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

CREATE INDEX IF NOT EXISTS idx_client_round_partial_sig_state_round_id
    ON client_round_partial_sig_state(round_id);

-- Client round collected VTXO forfeit signatures.
-- Stores the final signed forfeit transactions before they are submitted to
-- the server. This makes the crash window after collecting all VTXO actor
-- responses and checkpointing InputSigSentState restart-safe.
CREATE TABLE IF NOT EXISTS client_round_forfeit_sig_state (
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

CREATE INDEX IF NOT EXISTS idx_client_round_forfeit_sig_state_round_id
    ON client_round_forfeit_sig_state(round_id);

-- Client round expected VTXO forfeit requests.
-- Stores the request facts needed to re-ask local VTXO actors for signatures
-- after restart while a refresh/leave round is waiting in forfeit collection.
CREATE TABLE IF NOT EXISTS client_round_forfeit_request_state (
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

CREATE INDEX IF NOT EXISTS idx_client_round_forfeit_request_state_round_id
    ON client_round_forfeit_request_state(round_id);

-- Client round pending quotes.
-- Stores out-of-order JoinRoundQuoteReceived envelopes that were acknowledged
-- before the matching RoundJoined event re-keyed a temp FSM to the
-- server-assigned round id. This table intentionally does not reference
-- rounds: the round row usually does not exist yet when this fact is written.
CREATE TABLE IF NOT EXISTS client_round_pending_quotes (
    round_id TEXT PRIMARY KEY NOT NULL,
    quote_id BLOB NOT NULL CHECK(length(quote_id) = 32),
    seal_pass BIGINT NOT NULL,
    operator_fee_sat BIGINT NOT NULL,
    quote_expires_at BIGINT NOT NULL,
    reject_reason INTEGER NOT NULL,
    creation_time BIGINT NOT NULL,
    last_update_time BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_client_round_pending_quotes_created
    ON client_round_pending_quotes(creation_time, round_id);

CREATE TABLE IF NOT EXISTS client_round_pending_vtxo_quotes (
    round_id TEXT NOT NULL,
    quote_index INTEGER NOT NULL,
    pk_script BLOB NOT NULL,
    amount_sat BIGINT NOT NULL,
    recipient_key BLOB NOT NULL,

    PRIMARY KEY (round_id, quote_index),
    FOREIGN KEY (round_id) REFERENCES client_round_pending_quotes(round_id)
        ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS client_round_pending_leave_quotes (
    round_id TEXT NOT NULL,
    quote_index INTEGER NOT NULL,
    pk_script BLOB NOT NULL,
    amount_sat BIGINT NOT NULL,

    PRIMARY KEY (round_id, quote_index),
    FOREIGN KEY (round_id) REFERENCES client_round_pending_quotes(round_id)
        ON DELETE CASCADE
);

-- Client round effect rows record restart-safe work that must happen after a
-- round state transaction commits. The effect carries only routing identity;
-- effect handlers reconstruct payloads from normalized round tables.
CREATE TABLE IF NOT EXISTS client_round_effects (
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

CREATE INDEX IF NOT EXISTS idx_client_round_effects_round
    ON client_round_effects(round_id, status, created_at);

CREATE INDEX IF NOT EXISTS idx_client_round_effects_due
    ON client_round_effects(status, next_attempt_at, created_at);

-- VTXOs table.
-- Virtual Transaction Outputs owned by the client.
CREATE TABLE IF NOT EXISTS vtxos (
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
    tree_path BLOB NOT NULL,

    -- batch_expiry is the absolute block height at which the batch expires
    -- (when the operator can sweep via the batch-level timelock). Zero value
    -- is used for VTXOs created via the round store before the VTXO manager
    -- fills in the full metadata via ON CONFLICT DO UPDATE.
    batch_expiry INTEGER NOT NULL,

    -- tree_depth is the depth of this VTXO in the VTXT (used for expiry
    -- calculation based on TreeDepthMultiplier). Zero for same reason.
    tree_depth INTEGER NOT NULL,

    -- created_height is the block height when this VTXO was created.
    -- Zero for same reason.
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
    last_update_time BIGINT NOT NULL,

    PRIMARY KEY (outpoint_hash, outpoint_index),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id)
);

-- Index on round_id for lookup by round.
CREATE INDEX IF NOT EXISTS idx_vtxos_round_id
    ON vtxos(round_id);

-- Index on spent for listing unspent VTXOs.
CREATE INDEX IF NOT EXISTS idx_vtxos_spent
    ON vtxos(spent);

-- Index on creation_time for chronological queries.
CREATE INDEX IF NOT EXISTS idx_vtxos_creation_time
    ON vtxos(creation_time DESC);

-- Index on status for efficient status-based queries (ListLiveVTXOs, etc.).
CREATE INDEX IF NOT EXISTS idx_vtxos_status
    ON vtxos(status);
