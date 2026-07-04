-- Base infrastructure shared by every other domain: the chain identity
-- row, the normalized wallet key registry, and macaroon root keys.

-- Create a table to store blockchain information.
CREATE TABLE IF NOT EXISTS chain_info (
    id BIGINT PRIMARY KEY,
    chain_name TEXT NOT NULL UNIQUE,
    genesis_hash BLOB NOT NULL
);

-- Durable internal key registry.
--
-- Several client tables inline the same key-locator triple: a 33-byte
-- compressed pubkey BLOB plus a key_family / key_index pair (an lnd
-- keychain.KeyLocator). The triple lets us reconstruct the signing descriptor
-- for a wallet key, so it rides along with anything we have to re-sign or
-- re-derive after the fact. internal_keys is a normalized registry of every
-- such wallet key: each row is the full KeyDescriptor (pubkey + locator) and
-- consumer tables reference a row by a *_key_id foreign key instead of
-- re-spelling the three columns. Unlike the server registry, client keys have
-- no role concept, so the table is keyed purely by (pubkey, key_family,
-- key_index).
--
-- This table lives in the base migration (before boarding_addresses,
-- its earliest referencer) because Postgres requires the FK target table to
-- exist when a referencing constraint is declared.
--
-- BIGINT (not INTEGER) for key_family/key_index because keychain.KeyLocator
-- fields are uint32; Postgres INTEGER is signed 32-bit, so locator values above
-- MaxInt32 would not round-trip without sign-extension corruption. BIGINT
-- (signed 64-bit) safely covers the entire uint32 range.
CREATE TABLE IF NOT EXISTS internal_keys (
    -- id is the monotonically increasing surrogate key referenced by
    -- consumer tables' *_key_id foreign keys.
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    -- pubkey is the 33-byte compressed public key.
    pubkey BLOB NOT NULL,

    -- key_family and key_index are the lnd KeyLocator that lets the wallet
    -- reconstruct the signing descriptor for this key.
    key_family BIGINT NOT NULL,
    key_index BIGINT NOT NULL,

    -- created_at is the Unix timestamp when the key was first registered.
    created_at BIGINT NOT NULL,

    -- A given (pubkey, key_family, key_index) triple maps to exactly one
    -- canonical row. Registering the same triple again is idempotent and
    -- returns the existing id; this guard turns an inconsistent
    -- re-registration into a hard error rather than a silently divergent
    -- second row.
    UNIQUE (pubkey, key_family, key_index),

    -- Compressed secp256k1 public keys are exactly 33 bytes. length()
    -- returns the byte count for BLOB (SQLite) and BYTEA (Postgres).
    CHECK (length(pubkey) = 33)
);

-- Macaroon root keys for RPC authentication. id is the root key ID
-- (including the version prefix), root_key is the encrypted key
-- material.
CREATE TABLE IF NOT EXISTS macaroons (
    id BLOB PRIMARY KEY,
    root_key BLOB NOT NULL
);
