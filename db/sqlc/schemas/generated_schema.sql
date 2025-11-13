CREATE TABLE chain_info (
    id INTEGER PRIMARY KEY,
    chain_name TEXT NOT NULL UNIQUE,
    genesis_hash BLOB NOT NULL
);

