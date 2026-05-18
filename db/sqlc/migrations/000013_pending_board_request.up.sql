-- Records the user's explicit Board RPC intent so a daemon restart between
-- Board admission and round seal does not silently drop the request.
--
-- Rows are keyed by the confirmed boarding outpoint (outpoint_hash,
-- outpoint_index) that the original Board call admitted: one row per intent
-- the call covered. This binds each pending request to the specific UTXOs
-- the user pointed at, so a replay can never rebind a stale target_vtxo_count
-- to a fresh, unrelated boarding deposit. Rows are cleared when the matching
-- boarding intent transitions out of Confirmed (e.g. via the round-state
-- checkpoint that flips the intent to Adopted), so the cleanup runs in the
-- same SQL transaction as the state change rather than via a cross-actor
-- callback.
--
-- target_vtxo_count and requested_at_unix are denormalised onto every row
-- belonging to the same Board call. A subsequent Board call upserts the
-- value of target_vtxo_count for every still-Confirmed outpoint; rows for
-- already-adopted outpoints are unaffected because they have already been
-- cleared.
CREATE TABLE IF NOT EXISTS pending_board_requests (
    outpoint_hash BLOB NOT NULL,
    outpoint_index INTEGER NOT NULL,

    target_vtxo_count INTEGER NOT NULL DEFAULT 0,

    requested_at_unix BIGINT NOT NULL,

    PRIMARY KEY (outpoint_hash, outpoint_index),

    CHECK (target_vtxo_count >= 0),
    CHECK (requested_at_unix > 0)
);

-- Exact VTXO requests built for a pending Board RPC. The wallet's initial
-- pending_board_requests write happens before the round actor has derived
-- output owner/signing keys; the round actor fills this table before sending
-- JoinRoundRequest so a restart after server admission replays byte-for-byte
-- equivalent output intents instead of deriving fresh keys.
CREATE TABLE IF NOT EXISTS pending_board_vtxo_requests (
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
