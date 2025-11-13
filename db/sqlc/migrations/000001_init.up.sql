-- Initial schema for darepo database.
-- This migration creates the base chain_info table to track blockchain details.

-- Create a table to store blockchain information.
CREATE TABLE IF NOT EXISTS chain_info (
    id INTEGER PRIMARY KEY,
    chain_name TEXT NOT NULL UNIQUE,
    genesis_hash BLOB NOT NULL
);
