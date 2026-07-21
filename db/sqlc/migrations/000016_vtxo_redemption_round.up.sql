-- redemption_round_id records the claim-bearing round that durably adopted an
-- expired VTXO. It is deliberately separate from forfeit_round_id: claim
-- reissuance consumes an operator sweep liability, not the VTXO's cooperative
-- forfeit path.
ALTER TABLE vtxos
    ADD COLUMN redemption_round_id TEXT;

-- origin is local-only round output provenance. It must survive the durable
-- InputSigSent checkpoint so confirmation after a restart emits the same
-- ledger transition as uninterrupted confirmation.
ALTER TABLE round_vtxo_requests
    ADD COLUMN origin INTEGER NOT NULL DEFAULT 0;

-- sweep_delay is needed by post-checkpoint confirmation recovery to compute
-- the replacement's absolute batch expiry. Without it, a daemon restart after
-- signing would materialize the replacement with an already-expired height.
ALTER TABLE rounds
    ADD COLUMN sweep_delay BIGINT NOT NULL DEFAULT 0;

-- round_vtxo_claims preserves the claim authorization metadata that is not
-- part of an ordinary VTXO request. request_index binds each source to its
-- exact replacement request; the replacement signing descriptor remains in
-- round_vtxo_requests.signing_key_id so there is one authoritative key row.
CREATE TABLE round_vtxo_claims (
    round_id TEXT NOT NULL,
    request_index INTEGER NOT NULL,
    source_hash BLOB NOT NULL,
    source_index BIGINT NOT NULL,
    participant_pubkey BLOB NOT NULL,
    nonce BLOB NOT NULL,
    valid_from BIGINT NOT NULL,
    valid_until BIGINT NOT NULL,
    signature BLOB NOT NULL,

    PRIMARY KEY (round_id, source_hash, source_index),
    UNIQUE (round_id, request_index),
    FOREIGN KEY (round_id, request_index)
        REFERENCES round_vtxo_requests(round_id, request_index)
        ON DELETE CASCADE
);

-- vtxo_redemption_outbox closes the crash window between atomically linking
-- a Redeemed source to its replacement and durably enqueueing the accounting
-- plus live-actor materialization side effects. Rows are deleted only after
-- the observer accepts those idempotent effects, so startup can replay them
-- without consulting the operator again.
CREATE TABLE vtxo_redemption_outbox (
    source_hash BLOB NOT NULL,
    source_index INTEGER NOT NULL,
    replacement_hash BLOB NOT NULL,
    replacement_index INTEGER NOT NULL,
    redemption_round_id TEXT NOT NULL,
    creation_time BIGINT NOT NULL,

    PRIMARY KEY (source_hash, source_index)
);
