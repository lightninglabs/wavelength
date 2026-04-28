# arkrpc/treeconv

## Purpose

Thin facade providing public entry points for converting between the domain
`tree.Tree` type and the `arkrpc.TreePath` proto message. Callers import this
package rather than `arkrpc` directly so they remain decoupled from proto
package details.

## Key Types

- `TreePathFromTree(t *tree.Tree) (*arkrpc.TreePath, error)` — Converts a
  domain tree to its proto wire representation; delegates to
  `arkrpc.TreePathFromTree`.
- `TreePathToTree(tp *arkrpc.TreePath) (*tree.Tree, error)` — Converts a proto
  `TreePath` back to a domain `tree.Tree`; delegates to
  `arkrpc.TreePathToTree`.

## Relationships

- **Depends on**: `arkrpc` (underlying conversion logic), `lib/tree` (domain
  tree type).
- **Depended on by**: packages that need to serialize or deserialize `tree.Tree`
  for wire or storage without importing `arkrpc` directly.

## Invariants

- Both functions are thin forwarding wrappers; all conversion logic lives in
  `arkrpc`. Nil-safety is inherited from the underlying implementations.
- Do not add business logic here — this package is a module-boundary adapter
  only.

## Deep Docs

- [arkrpc/CLAUDE.md](../CLAUDE.md) — Parent arkrpc package with full conversion details.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
