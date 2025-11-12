-- Initial schema for darepo database.
-- This migration creates the base chain_info table to track blockchain details.

-- Create a table to store blockchain information.
CREATE TABLE IF NOT EXISTS chain_info (
    id INTEGER PRIMARY KEY,
    chain_name TEXT NOT NULL UNIQUE,
    genesis_hash BLOB NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Insert Bitcoin mainnet information as an example.
-- Genesis hash for Bitcoin mainnet:
-- 000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f
INSERT INTO chain_info (id, chain_name, genesis_hash) VALUES (
    1,
    'mainnet',
    X'000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f'
);
