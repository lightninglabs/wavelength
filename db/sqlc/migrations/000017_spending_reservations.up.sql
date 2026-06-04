-- spending_reservations is a durable index of VTXO outpoints currently held
-- in SpendingState by an active spend owner (e.g. an outgoing OOR session).
-- A row exists IFF the owning session was durably checkpointed, so a startup
-- sweep can deterministically identify orphaned Spending VTXOs (those with no
-- reservation row) and release them.
CREATE TABLE IF NOT EXISTS spending_reservations (
    -- outpoint_hash identifies the reserved VTXO outpoint. The 32-byte
    -- length check rejects truncated or malformed hashes at the DB layer.
    outpoint_hash BLOB NOT NULL CHECK (length(outpoint_hash) = 32),

    -- outpoint_index is the output index of the reserved outpoint.
    outpoint_index INTEGER NOT NULL CHECK (outpoint_index >= 0),

    -- owner_kind encodes the reservation owner type:
    --   0 = oor outgoing session
    owner_kind INTEGER NOT NULL,

    -- owner_id is the owner's stable identifier (e.g. the OOR session id, a
    -- 32-byte hash). The length check mirrors outpoint_hash.
    owner_id BLOB NOT NULL CHECK (length(owner_id) = 32),

    -- created_at is the unix timestamp when the reservation was created.
    created_at BIGINT NOT NULL,

    -- Primary key keeps one reservation row per reserved outpoint.
    PRIMARY KEY (outpoint_hash, outpoint_index)
);

-- Backfill reservations for VTXOs already held in SpendingState at upgrade
-- time. Without this, a node upgrading from schema 16 with an in-flight
-- outgoing OOR session would start with an empty index, and the startup
-- orphan sweep would see those legitimately Spending VTXOs as row-less
-- orphans and release them, freeing inputs the checkpointed send still owns.
--
-- The owning session id is not extractable from the OOR checkpoint blob in
-- SQL, and the sweep only consults the outpoint set, so we use the VTXO's own
-- outpoint_hash as a 32-byte owner_id placeholder (owner_kind 0 = oor
-- outgoing). status 7 is VTXOStatusSpending. ON CONFLICT keeps the migration
-- safe to re-run against a dirty version.
INSERT INTO spending_reservations (
    outpoint_hash, outpoint_index, owner_kind, owner_id, created_at
)
SELECT outpoint_hash, outpoint_index, 0, outpoint_hash, 0
FROM vtxos
WHERE status = 7
ON CONFLICT (outpoint_hash, outpoint_index) DO NOTHING;
