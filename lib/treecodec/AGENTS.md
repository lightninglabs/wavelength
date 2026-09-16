# lib/treecodec

## Purpose

Shared TLV serialization for complete Bitcoin and asset transaction trees.
Database rows and durable actor checkpoints use the same representation.

## Key Types and Functions

- `SerializeTree` preserves the existing database and ancestry identity bytes.
- `SerializeSnapshot` additionally records cached subtree amounts.
- `DeserializeTree` restores the tree with depth and child-count bounds.
- `MaxTreeDeserializeDepth` and `MaxTreeChildrenPerNode` bound decoding.

## Relationships

Depends on `lib/tree`, wire transactions, and TLV primitives. The database
preserves its existing codec API through wrappers around this package.

## Invariants

- The legacy encoding remains byte-compatible with database tree blobs.
- Snapshot-only cached amounts never change ancestry fragment identities.
- Child serialization is deterministic.
- The complete representation retains cached final signing keys.
- Asset context is validated before encoding and after restoration.

## Deep Docs

See `lib/tree/AGENTS.md` for tree ownership and immutability rules.
