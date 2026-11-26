-- The shared fee broadcaster has one pending wallet split. Store its signed
-- bytes and output assignments before the first publication attempt.
CREATE TABLE wallet_fee_split (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    candidate BLOB NOT NULL
);
