-- Register the append-only source code before any alias row can reference it.
INSERT INTO owned_receive_script_sources (source, name) VALUES
    (3, 'asset_alias')
ON CONFLICT DO NOTHING;

-- SQLite cannot alter a CHECK constraint in place, so rebuild the table.
-- This shape is also valid PostgreSQL and preserves every existing row.
CREATE TABLE owned_receive_scripts_asset_alias (
    -- pk_script is the owned receive script primary key.
    pk_script BLOB PRIMARY KEY NOT NULL,

    -- client_key_id references the internal_keys registry row for the client
    -- wallet key used in the checkpoint taptree. The registry row carries the
    -- compressed pubkey plus the lnd KeyLocator. Declared nullable only for
    -- uniformity with the genuinely-optional internal_keys FKs (vtxos,
    -- round_vtxo_requests); in practice every owned receive script has a
    -- client key, so the write path always registers it first and the read
    -- path treats a NULL as an error.
    client_key_id BIGINT REFERENCES internal_keys(id),

    -- operator_pubkey is the operator key used in the checkpoint taptree.
    operator_pubkey BLOB NOT NULL,

    -- exit_delay is the CSV delay used in the timeout branch.
    exit_delay BIGINT NOT NULL,

    -- source labels how this script was discovered/registered:
    --   0 = wallet
    --   1 = rpc
    --   2 = sync
    --   3 = final Taproot Asset output alias
    source INTEGER NOT NULL CHECK (source IN (0, 1, 2, 3)),

    -- created_at is the unix timestamp when this script was registered.
    created_at BIGINT NOT NULL,

    -- last_used_at is an optional unix timestamp of latest usage.
    last_used_at BIGINT,

    -- idempotency_key identifies one durable receive-script allocation
    -- across retries; NULL keeps the legacy allocate-a-fresh-script
    -- behavior.
    idempotency_key TEXT,

    -- registration_label is the immutable label the allocation was
    -- admitted with.
    registration_label TEXT,

    -- registration_expires_at is the absolute indexer registration
    -- expiry.
    registration_expires_at BIGINT,

    -- registration_rpc_key is the stable mailbox RPC correlation key.
    registration_rpc_key TEXT,

    -- registration_completed_at records completion evidence for a
    -- finished admission.
    registration_completed_at BIGINT,

    -- Source enum foreign key.
    FOREIGN KEY (source) REFERENCES owned_receive_script_sources(source)
);

INSERT INTO owned_receive_scripts_asset_alias (
    pk_script, client_key_id, operator_pubkey, exit_delay, source,
    created_at, last_used_at, idempotency_key, registration_label,
    registration_expires_at, registration_rpc_key,
    registration_completed_at
)
SELECT
    pk_script, client_key_id, operator_pubkey, exit_delay, source,
    created_at, last_used_at, idempotency_key, registration_label,
    registration_expires_at, registration_rpc_key,
    registration_completed_at
FROM owned_receive_scripts;

DROP TABLE owned_receive_scripts;

ALTER TABLE owned_receive_scripts_asset_alias
    RENAME TO owned_receive_scripts;

-- The rebuild dropped the index with the old table; restore it so one
-- durable allocation per non-null idempotency key stays enforced.
CREATE UNIQUE INDEX idx_owned_receive_scripts_idempotency_key
    ON owned_receive_scripts(idempotency_key)
    WHERE idempotency_key IS NOT NULL;
