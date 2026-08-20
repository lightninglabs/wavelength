-- Preserve the optional Taproot Asset commitment root beside each VTXO's
-- semantic Ark policy. A NULL root denotes the historical Bitcoin-only
-- output. The 32-byte root is sufficient to reconstruct every Ark control
-- block without persisting taproot-assets implementation types.
ALTER TABLE vtxos ADD COLUMN taproot_asset_root BLOB;
