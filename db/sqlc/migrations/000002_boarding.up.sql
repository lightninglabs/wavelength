-- Boarding lifecycle: addresses, intents, and on-chain sweeps of
-- boarding deposits.

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
    (4, 'swept'),
    (5, 'sweep_pending');

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

    -- client_key_id references the internal_keys registry row for the client
    -- wallet key used in the tapscript. The registry row carries the
    -- compressed pubkey plus the lnd KeyLocator needed to reconstruct the
    -- signing descriptor. Declared nullable only for uniformity with the
    -- genuinely-optional internal_keys FKs (vtxos, round_vtxo_requests); in
    -- practice every boarding address has a client key, so the write path
    -- always registers it first and the read path treats a NULL as an error.
    client_key_id BIGINT REFERENCES internal_keys(id),

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

    -- tx_proof is the SPV TxProof built when the boarding UTXO first
    -- confirms, persisted so it survives a daemon restart and can be
    -- replayed to the round actor (and onward to the server) without
    -- rebuilding it from chain. The wire format matches
    -- round_boarding_intents.tx_proof: a TLV encoding produced by
    -- lib/types.SerializeTxProof. NULL when no proof has been built
    -- yet (None on read).
    tx_proof BLOB,

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

-- Boarding sweep tracking.
--
-- A broadcast boarding sweep is not complete until the chain backend reports
-- the swept boarding outpoints as spent. These tables persist the published
-- sweep transaction and each input so the daemon can resume spend watches and
-- rebroadcast the exact same transaction after restart.

CREATE TABLE IF NOT EXISTS boarding_sweeps (
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

CREATE INDEX IF NOT EXISTS idx_boarding_sweeps_status
    ON boarding_sweeps(status);

CREATE TABLE IF NOT EXISTS boarding_sweep_inputs (
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

CREATE INDEX IF NOT EXISTS idx_boarding_sweep_inputs_status
    ON boarding_sweep_inputs(status);

CREATE INDEX IF NOT EXISTS idx_boarding_sweep_inputs_outpoint
    ON boarding_sweep_inputs(outpoint_hash, outpoint_index);

CREATE UNIQUE INDEX IF NOT EXISTS idx_boarding_sweep_inputs_active_outpoint
    ON boarding_sweep_inputs(outpoint_hash, outpoint_index)
    WHERE status IN ('pending', 'published');
