-- round_asset_deposits links an asset deposit to a round that checkpointed
-- with it as a boarding input. round_boarding_intents references the Bitcoin
-- boarding tables, which deposits stay out of, so deposits get their own link.
CREATE TABLE IF NOT EXISTS round_asset_deposits (
    round_id TEXT NOT NULL REFERENCES rounds(round_id) ON DELETE CASCADE,
    request_id TEXT NOT NULL REFERENCES asset_deposits(request_id),

    -- input_index and input_signature are the client's signature of the
    -- deposit input, which a restarted client needs to resume the round.
    input_index INTEGER NOT NULL CHECK (input_index >= 0),
    input_signature BLOB NOT NULL,

    PRIMARY KEY (round_id, request_id)
);

CREATE INDEX IF NOT EXISTS idx_round_asset_deposits_request_id
    ON round_asset_deposits(request_id);
