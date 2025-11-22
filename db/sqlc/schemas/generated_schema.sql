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
    last_update_time BIGINT NOT NULL,

    PRIMARY KEY (outpoint_hash, outpoint_index),
    FOREIGN KEY (pk_script) REFERENCES boarding_addresses(pk_script),
    FOREIGN KEY (status) REFERENCES boarding_statuses(status_name)
);

CREATE TABLE boarding_statuses (
    id BIGINT PRIMARY KEY,
    status_name TEXT UNIQUE NOT NULL
);

CREATE TABLE chain_info (
    id BIGINT PRIMARY KEY,
    chain_name TEXT NOT NULL UNIQUE,
    genesis_hash BLOB NOT NULL
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

