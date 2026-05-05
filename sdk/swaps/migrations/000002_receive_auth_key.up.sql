-- receive_auth_keys stores the stable locally generated key used to sign
-- Lightning-to-Ark receive invoices and decode their forwarded onions.
CREATE TABLE IF NOT EXISTS receive_auth_keys (
    key_id TEXT PRIMARY KEY,
    private_key BLOB NOT NULL,
    created_at_unix BIGINT NOT NULL DEFAULT (strftime('%s', 'now'))
);
