-- Indexer receive script registrations.
--
-- This table binds receive scripts to mailbox principals so the indexer can
-- route wallet-scoped event notifications through mailbox delivery.
--
-- NOTE: This schema is written to be compatible with both sqlite and postgres.

CREATE TABLE IF NOT EXISTS indexer_receive_scripts (
    -- principal_mailbox_id is the canonical mailbox id of the authenticated
    -- wallet principal (for example, "client:<id>").
    principal_mailbox_id TEXT NOT NULL,

    -- pk_script is the receive script bytes controlled by the principal.
    pk_script BLOB NOT NULL,

    -- expires_at_unix_s is an optional unix timestamp (seconds) after which
    -- this registration should be treated as inactive. Zero means no expiry.
    expires_at_unix_s BIGINT NOT NULL DEFAULT 0,

    -- label is optional client-provided metadata for debugging and UX.
    label TEXT NOT NULL DEFAULT '',

    -- updated_at is the unix nano timestamp of the latest registration write.
    updated_at BIGINT NOT NULL,

    -- owner_pubkey is the optional compressed owner pubkey for standard Ark
    -- VTXO receive scripts.
    owner_pubkey BLOB,

    -- operator_pubkey is the optional compressed operator pubkey committed to
    -- the standard Ark VTXO receive script.
    operator_pubkey BLOB,

    -- exit_delay is the optional CSV delay committed to the standard Ark VTXO
    -- receive script.
    exit_delay BIGINT,

    PRIMARY KEY(principal_mailbox_id, pk_script)
);

-- Index used by event fanout lookups (pk_script -> principals).
CREATE INDEX IF NOT EXISTS idx_indexer_receive_scripts_script
    ON indexer_receive_scripts(pk_script);
