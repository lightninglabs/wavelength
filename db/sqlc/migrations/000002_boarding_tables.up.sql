-- Boarding tables migration.
-- This migration creates tables for boarding address and boarding intent management.

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
-- This table is created first in this migration (before boarding_addresses,
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

-- Enum-like table for boarding intent lifecycle states.
-- From the wallet's perspective, intents start in 'confirmed' state since they
-- are only created after a UTXO has been confirmed on-chain.
CREATE TABLE IF NOT EXISTS boarding_statuses (
    id BIGINT PRIMARY KEY,
    status_name TEXT UNIQUE NOT NULL
);

-- Populate the possible boarding statuses.
INSERT INTO boarding_statuses (id, status_name) VALUES
    (0, 'confirmed'),
    (1, 'adopted'),
    (2, 'failed'),
    (3, 'expired'),
    (4, 'swept');

-- Create table to store boarding addresses.
-- Boarding addresses are taproot addresses that clients generate to receive
-- boarding funds. Each address includes the keys and CSV timelock parameters
-- needed to construct collaborative and timeout spending paths.
-- The tapscript is reconstructed on read from the stored component fields
-- (client_pubkey, operator_pubkey, exit_delay) using scripts.VTXOTapScript().
CREATE TABLE IF NOT EXISTS boarding_addresses (
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

-- Create index on last_confirmed_height for efficient queries during startup.
CREATE INDEX IF NOT EXISTS idx_boarding_addresses_last_confirmed
    ON boarding_addresses(last_confirmed_height DESC);

-- Create index on creation_time for chronological queries.
CREATE INDEX IF NOT EXISTS idx_boarding_addresses_creation_time
    ON boarding_addresses(creation_time DESC);

-- Create table to store boarding intents.
-- Boarding intents track the lifecycle of a specific boarding attempt from
-- on-chain confirmation through round completion. Intents are only created
-- after the boarding UTXO has been confirmed on-chain, so conf_height and
-- conf_hash are always present.
CREATE TABLE IF NOT EXISTS boarding_intents (
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
    last_update_time BIGINT NOT NULL,

    PRIMARY KEY (outpoint_hash, outpoint_index),
    FOREIGN KEY (pk_script) REFERENCES boarding_addresses(pk_script),
    FOREIGN KEY (status) REFERENCES boarding_statuses(status_name)
);

-- Create index on pk_script for efficient lookup of intents by address.
CREATE INDEX IF NOT EXISTS idx_boarding_intents_pk_script
    ON boarding_intents(pk_script);

-- Create index on status for efficient queries by lifecycle stage.
CREATE INDEX IF NOT EXISTS idx_boarding_intents_status
    ON boarding_intents(status);

-- Create index on conf_height for efficient range queries and startup backlog.
CREATE INDEX IF NOT EXISTS idx_boarding_intents_conf_height
    ON boarding_intents(conf_height DESC);

-- Create index on creation_time for chronological queries.
CREATE INDEX IF NOT EXISTS idx_boarding_intents_creation_time
    ON boarding_intents(creation_time DESC);
