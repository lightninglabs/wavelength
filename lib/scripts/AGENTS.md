# lib/scripts

## Purpose

**Removed.** This package previously provided low-level Bitcoin script
construction helpers for standard Ark outputs (VTXO and checkpoint
tapscripts). All functionality has been migrated to `lib/arkscript`, which
provides a policy-based tapscript compiler with the same canonical output.

See `lib/arkscript` for `VTXOPolicy`, `CheckpointPolicy`, `SpendInfo`,
`AnchorOutput`, and related types that replace the former contents of this
package.

## Deep Docs

- [lib/arkscript/CLAUDE.md](../arkscript/CLAUDE.md) — Replacement package.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
