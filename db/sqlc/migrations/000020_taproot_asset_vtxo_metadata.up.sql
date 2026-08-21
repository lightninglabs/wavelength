-- Keep SDK-neutral Taproot Asset identity and quantity beside each asset
-- commitment root. The amount uses an exactly eight-byte, big-endian BLOB so
-- the complete unsigned 64-bit Taproot Asset range survives both SQLite and
-- PostgreSQL instead of narrowing through SQL BIGINT.
ALTER TABLE vtxos ADD COLUMN taproot_asset_ref TEXT;
ALTER TABLE vtxos ADD COLUMN taproot_asset_amount BLOB;

-- An asset VTXO created inside a round carries the sealed tap-sdk package
-- that created it. Spending it out of round needs that package to rebuild
-- the compact proof path and the OP_TRUE witness, and the owner has no
-- other source for either: the operator holds every tree node's package.
ALTER TABLE vtxos ADD COLUMN taproot_asset_sealed_package BLOB;
