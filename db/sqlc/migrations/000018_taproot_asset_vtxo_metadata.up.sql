-- Keep SDK-neutral Taproot Asset identity and quantity beside each asset
-- commitment root. The amount uses an exactly eight-byte, big-endian BLOB so
-- the complete unsigned 64-bit Taproot Asset range survives both SQLite and
-- PostgreSQL instead of narrowing through SQL BIGINT.
ALTER TABLE vtxos ADD COLUMN taproot_asset_ref TEXT;
ALTER TABLE vtxos ADD COLUMN taproot_asset_amount BLOB;
