-- Retain the immutable Taproot Asset transition container beside the
-- finalized Bitcoin package. NULL denotes a Bitcoin-only OOR session.
ALTER TABLE oor_packages ADD COLUMN taproot_asset_transfer BLOB;
