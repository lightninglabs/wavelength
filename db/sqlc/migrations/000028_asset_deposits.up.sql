-- asset_deposits tracks Taproot Asset boarding outputs this daemon funds
-- from its tapd wallet, from the first funding attempt until the output's
-- proof is confirmed. They stay out of boarding_addresses and
-- boarding_intents, so Bitcoin boarding and boarding sweeps never select an
-- output whose asset they cannot carry.
CREATE TABLE IF NOT EXISTS asset_deposits (
    -- request_id is the caller's idempotency key and the onboarding journal
    -- key.
    request_id TEXT PRIMARY KEY
        CHECK (length(request_id) BETWEEN 1 AND 128),

    -- status is funding until tapd published the output, published until
    -- its proof confirmed, then confirmed. A failed funding attempt stays
    -- funding with last_error set, and a retry replays it.
    status TEXT NOT NULL
        CHECK (status IN ('funding', 'published', 'confirmed')),

    -- The request below is replayed verbatim on retry and restart, because
    -- the onboarding journal is bound to its digest.
    asset_ref TEXT NOT NULL CHECK (length(asset_ref) BETWEEN 1 AND 512),

    -- asset_amount is a big-endian uint64, so the full range survives on
    -- SQLite and PostgreSQL.
    asset_amount BLOB NOT NULL CHECK (length(asset_amount) = 8),
    carrier_value_sat BIGINT NOT NULL CHECK (carrier_value_sat > 0),
    fee_rate_sat_per_vbyte BIGINT NOT NULL CHECK (fee_rate_sat_per_vbyte >= 0),
    target_conf INTEGER NOT NULL CHECK (target_conf >= 0),
    max_fee_sat BIGINT NOT NULL CHECK (max_fee_sat >= 0),
    operator_key BLOB NOT NULL CHECK (length(operator_key) = 33),
    exit_delay INTEGER NOT NULL CHECK (exit_delay > 0),

    -- funding_proofs is the witness-vector encoding of the confirmed proof
    -- files that fund the output.
    funding_proofs BLOB NOT NULL,

    -- The published output and the claim that boards it. All set once
    -- status leaves funding.
    outpoint_hash BLOB CHECK (outpoint_hash IS NULL OR length(outpoint_hash) = 32),
    outpoint_index INTEGER,
    value_sat BIGINT,
    fee_sat BIGINT,
    pk_script BLOB,
    policy_template BLOB,
    owner_key_family INTEGER,
    owner_key_index INTEGER,
    owner_pubkey BLOB CHECK (owner_pubkey IS NULL OR length(owner_pubkey) = 33),
    digest BLOB CHECK (digest IS NULL OR length(digest) = 32),
    script_key BLOB CHECK (script_key IS NULL OR length(script_key) = 33),
    taproot_asset_root BLOB
        CHECK (taproot_asset_root IS NULL OR length(taproot_asset_root) = 32),
    op_true_witness BLOB,

    -- proof_file is the output's confirmed proof, set with confirmed.
    proof_file BLOB,
    confirmed_height INTEGER,

    -- last_error is the most recent failure, empty when none.
    last_error TEXT NOT NULL DEFAULT '',

    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,

    CHECK (
        status = 'funding' OR (
            outpoint_hash IS NOT NULL AND outpoint_index IS NOT NULL
            AND value_sat IS NOT NULL AND fee_sat IS NOT NULL
            AND pk_script IS NOT NULL AND policy_template IS NOT NULL
            AND owner_key_family IS NOT NULL
            AND owner_key_index IS NOT NULL
            AND owner_pubkey IS NOT NULL AND digest IS NOT NULL
            AND script_key IS NOT NULL AND taproot_asset_root IS NOT NULL
            AND op_true_witness IS NOT NULL
        )
    ),
    CHECK (status != 'confirmed' OR (
        proof_file IS NOT NULL AND confirmed_height IS NOT NULL
    ))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_asset_deposits_outpoint
    ON asset_deposits(outpoint_hash, outpoint_index)
    WHERE outpoint_hash IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_asset_deposits_status
    ON asset_deposits(status);
