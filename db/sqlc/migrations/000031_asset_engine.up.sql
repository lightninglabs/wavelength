-- The embedded asset engine keeps what a tapd would keep in its own
-- database: the keys it derived from the wallet seed, the proof files of
-- asset outputs, and the transfers it published.

-- asset_engine_keys holds the anchor and script base keys the engine
-- derived, with the derivation path that recovers their private keys.
CREATE TABLE IF NOT EXISTS asset_engine_keys (
    pubkey BLOB PRIMARY KEY CHECK (length(pubkey) = 33),
    key_family BIGINT NOT NULL CHECK (key_family >= 0),
    key_index BIGINT NOT NULL CHECK (key_index >= 0)
);

-- asset_engine_script_keys maps the tweaked key of an asset output to the
-- key it is tweaked from. A declared key the wallet cannot derive has a
-- zero derivation path.
CREATE TABLE IF NOT EXISTS asset_engine_script_keys (
    tweaked_key BLOB PRIMARY KEY CHECK (length(tweaked_key) = 33),
    raw_key BLOB NOT NULL CHECK (length(raw_key) = 33),
    key_family BIGINT NOT NULL CHECK (key_family >= 0),
    key_index BIGINT NOT NULL CHECK (key_index >= 0),

    -- tweak is NULL for a BIP-86 key.
    tweak BLOB,
    key_type INTEGER NOT NULL CHECK (key_type >= 0)
);

-- asset_engine_proofs holds the confirmed proof file of an asset output.
-- owned marks an output the wallet holds, and spent an owned output that a
-- recorded transfer spends.
CREATE TABLE IF NOT EXISTS asset_engine_proofs (
    asset_id BLOB NOT NULL CHECK (length(asset_id) = 32),
    script_key BLOB NOT NULL CHECK (length(script_key) = 33),
    outpoint_hash BLOB NOT NULL CHECK (length(outpoint_hash) = 32),
    outpoint_index BIGINT NOT NULL CHECK (outpoint_index >= 0),
    proof_file BLOB NOT NULL,
    owned BOOLEAN NOT NULL,
    spent BOOLEAN NOT NULL DEFAULT FALSE,

    PRIMARY KEY (asset_id, script_key, outpoint_hash, outpoint_index)
);

CREATE INDEX IF NOT EXISTS idx_asset_engine_proofs_owned
    ON asset_engine_proofs(owned, spent);

-- asset_engine_transfers holds the transfers the engine published. state is
-- 0 while the anchor transaction is unconfirmed, 1 once its final proofs are
-- stored, and 2 once the confirmation is final. payload is the engine's own
-- encoding of the transfer.
CREATE TABLE IF NOT EXISTS asset_engine_transfers (
    anchor_txid BLOB PRIMARY KEY CHECK (length(anchor_txid) = 32),
    state INTEGER NOT NULL CHECK (state BETWEEN 0 AND 2),
    height_hint BIGINT NOT NULL CHECK (height_hint >= 0),

    -- block_hash is NULL while the transfer is unconfirmed.
    block_hash BLOB CHECK (block_hash IS NULL OR length(block_hash) = 32),
    block_height BIGINT NOT NULL DEFAULT 0 CHECK (block_height >= 0),

    -- created_at is in Unix nanoseconds and orders the transfers.
    created_at BIGINT NOT NULL,
    payload BLOB NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_asset_engine_transfers_created_at
    ON asset_engine_transfers(created_at);

-- asset_journal holds the idempotency journal of asset deposits made with
-- the embedded backend, which a browser cannot keep in files.
CREATE TABLE IF NOT EXISTS asset_journal (
    journal_key TEXT PRIMARY KEY CHECK (length(journal_key) BETWEEN 1 AND 256),
    value BLOB NOT NULL
);
