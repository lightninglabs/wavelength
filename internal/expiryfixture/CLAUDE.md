# internal/expiryfixture

## Purpose

Builds signed VTXO ancestry for acceptance-boundary tests. Tests that exercise
authenticated expiry need a VTXO whose commitment transaction, tree node, and
signatures actually verify; hand-rolling that in each test package duplicated
a lot of fragile setup. This package produces those fixtures deterministically
from a single caller-supplied tag.

## Key Types

- `Round(t, value, script, delay, height, tag)` — Returns a signed
  round-direct `arkrpc.VTXO` inventory entry and its commitment transaction.
  The `tag` seeds the owner and operator keys and the commitment input hash,
  so independent fixtures get distinct but reproducible commitment IDs.
- `Merge(t, sameBatch)` — Returns a VTXO whose ancestry merges two parents,
  plus the transactions backing it. `sameBatch` selects whether the two
  parents share one commitment or come from separate batches.

## Relationships

- **Depends on**: `arkrpc` (the `VTXO` inventory message shape),
  `lib/arkscript` (unilateral CSV timeout leaf, anchor pkScript), `lib/tree`
  (node construction, `ComputeFinalKey`), `lib/tx/psbtutil` (PSBT assembly and
  signature attachment).
- **Depended on by**: test files in `vtxo` (expiry target, incoming ancestry
  resolver), `oor` (incoming metadata query, receive limits), and `waved`
  (incoming metadata, wallet recovery descriptor and OOR family).
- **Sends** / **Receives**: none — synchronous test helpers, no actor traffic.

## Invariants

- Test-only. Every entry point takes a `testing.TB` and fails the test via
  `require` rather than returning an error; never call these from production
  paths.
- Fixtures are deterministic: the same `tag` always yields the same keys,
  commitment ID, and signatures. Two fixtures that must not collide in a test
  need different tags.
- The MuSig2 signatures produced here are real and must verify — a change to
  `lib/tree` key aggregation or `lib/arkscript` leaf construction will surface
  as failures in the consuming packages, not here.

## Deep Docs

- [internal/CLAUDE.md](../CLAUDE.md) — Parent internal package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
