-- Asset aliases are derived receiver state. Drop them before restoring the
-- older source constraint; the registered semantic receive scripts remain.
DELETE FROM owned_receive_scripts WHERE source = 3;

CREATE TABLE owned_receive_scripts_without_asset_alias (
    -- pk_script is the owned receive script primary key.
    pk_script BLOB PRIMARY KEY NOT NULL,

    -- client_key_id references the internal_keys registry row for the client
    -- wallet key used in the checkpoint taptree.
    client_key_id BIGINT REFERENCES internal_keys(id),

    -- operator_pubkey is the operator key used in the checkpoint taptree.
    operator_pubkey BLOB NOT NULL,

    -- exit_delay is the CSV delay used in the timeout branch.
    exit_delay BIGINT NOT NULL,

    -- Restore the original append-only source range.
    source INTEGER NOT NULL CHECK (source IN (0, 1, 2)),

    -- created_at is the unix timestamp when this script was registered.
    created_at BIGINT NOT NULL,

    -- last_used_at is an optional unix timestamp of latest usage.
    last_used_at BIGINT,

    -- Source enum foreign key.
    FOREIGN KEY (source) REFERENCES owned_receive_script_sources(source)
);

INSERT INTO owned_receive_scripts_without_asset_alias (
    pk_script, client_key_id, operator_pubkey, exit_delay, source,
    created_at, last_used_at
)
SELECT
    pk_script, client_key_id, operator_pubkey, exit_delay, source,
    created_at, last_used_at
FROM owned_receive_scripts;

DROP TABLE owned_receive_scripts;

ALTER TABLE owned_receive_scripts_without_asset_alias
    RENAME TO owned_receive_scripts;

DELETE FROM owned_receive_script_sources WHERE source = 3;
