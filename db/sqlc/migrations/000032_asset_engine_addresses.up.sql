-- The asset engine receives at Taproot Asset addresses. A sender announces
-- a transfer in the mailbox of the address's proof courier, and the engine
-- records each announcement before it fetches and verifies the proofs.

-- asset_engine_addresses holds the addresses the engine created. script_key
-- is the raw script key of the address, which names its mailbox.
CREATE TABLE IF NOT EXISTS asset_engine_addresses (
    -- address_id orders addresses created at the same time.
    address_id INTEGER PRIMARY KEY AUTOINCREMENT,
    script_key BLOB NOT NULL UNIQUE CHECK (length(script_key) = 33),
    encoded TEXT NOT NULL CHECK (length(encoded) > 0),
    internal_key BLOB NOT NULL CHECK (length(internal_key) = 33),

    -- creation_height is the chain height at creation. No transfer to the
    -- address confirms below it.
    creation_height BIGINT NOT NULL CHECK (creation_height >= 0),

    -- created_at is in Unix nanoseconds.
    created_at BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_asset_engine_addresses_created_at
    ON asset_engine_addresses(created_at, address_id);

-- asset_engine_receives holds one row per announcement of a transfer to an
-- address. Anyone who knows an address can announce an output, so one output
-- can have several rows. state is 0 for an announced transfer, 1 once its
-- proofs are stored as outputs of the wallet, and 2 for a transfer the
-- wallet took no asset from. The engine defines later states. payload is the
-- engine's own encoding of the receive.
CREATE TABLE IF NOT EXISTS asset_engine_receives (
    -- receive_id orders receives recorded at the same time.
    receive_id INTEGER PRIMARY KEY AUTOINCREMENT,
    outpoint_hash BLOB NOT NULL CHECK (length(outpoint_hash) = 32),
    outpoint_index BIGINT NOT NULL CHECK (outpoint_index >= 0),

    -- address_script_key is the script_key of the paid address.
    address_script_key BLOB NOT NULL
        CHECK (length(address_script_key) = 33),

    -- announcement is the engine's hash of what the sender announced.
    announcement BLOB NOT NULL CHECK (length(announcement) = 32),
    state INTEGER NOT NULL CHECK (state >= 0),

    -- block_hash is NULL until the receive is confirmed.
    block_hash BLOB CHECK (block_hash IS NULL OR length(block_hash) = 32),
    block_height BIGINT NOT NULL DEFAULT 0 CHECK (block_height >= 0),

    -- created_at is in Unix nanoseconds.
    created_at BIGINT NOT NULL,
    payload BLOB NOT NULL,

    UNIQUE (outpoint_hash, outpoint_index, address_script_key, announcement)
);

CREATE INDEX IF NOT EXISTS idx_asset_engine_receives_created_at
    ON asset_engine_receives(created_at, receive_id);
