-- Keep asset units separate from the Bitcoin carrier. An eight-byte BLOB
-- preserves the complete uint64 amount range on SQLite and PostgreSQL.
ALTER TABLE vtxos ADD COLUMN taproot_asset_root BLOB
    CHECK (taproot_asset_root IS NULL OR length(taproot_asset_root) = 32);
ALTER TABLE vtxos ADD COLUMN taproot_asset_ref TEXT
    CHECK (taproot_asset_ref IS NULL OR length(taproot_asset_ref) BETWEEN 1 AND 512);
ALTER TABLE vtxos ADD COLUMN taproot_asset_amount BLOB
    CHECK (taproot_asset_amount IS NULL OR length(taproot_asset_amount) = 8);
ALTER TABLE vtxos ADD COLUMN taproot_asset_sealed_package BLOB
    CHECK (
        (taproot_asset_root IS NULL AND taproot_asset_ref IS NULL
         AND taproot_asset_amount IS NULL
         AND taproot_asset_sealed_package IS NULL)
        OR
        (taproot_asset_root IS NOT NULL AND taproot_asset_ref IS NOT NULL
         AND taproot_asset_amount IS NOT NULL)
    );
