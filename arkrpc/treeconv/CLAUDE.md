# arkrpc/treeconv

## Purpose

Thin adapter package that re-exports the two tree-path conversion helpers
from `arkrpc` under a dedicated, narrow import path. Callers that only
need tree serialization can import `arkrpc/treeconv` without pulling in the
full `arkrpc` gRPC surface.

## Key Types

- `TreePathFromTree(t *tree.Tree) (*arkrpc.TreePath, error)` — Delegates
  to `arkrpc.TreePathFromTree`; converts a `lib/tree.Tree` to its proto
  wire representation using deterministic pre-order flattening with sorted
  child indices.
- `TreePathToTree(tp *arkrpc.TreePath) (*tree.Tree, error)` — Delegates to
  `arkrpc.TreePathToTree`; converts a proto `TreePath` back to a
  `lib/tree.Tree`.

## Relationships

- **Depends on**: `arkrpc` (conversion logic and `TreePath` proto type),
  `lib/tree` (domain `Tree` type).
- **Depended on by**: packages that need tree serialization without the
  full `arkrpc` gRPC import (indexer, serverconn, oor consumers).

## Invariants

- Round-trip invariant (inherited from `arkrpc`): `TreePathFromTree` →
  `TreePathToTree` must reproduce the original tree, excluding derived
  `FinalKey` fields.
- All logic lives in `arkrpc`; this package contains only forwarding
  declarations — never add business logic here.

## Deep Docs

- [arkrpc/CLAUDE.md](../CLAUDE.md) — Parent package with full conversion details.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
