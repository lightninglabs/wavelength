# expiryfixture

## Purpose

Test-only fixture package that builds *signed* VTXO ancestry for
acceptance-boundary tests. It produces `arkrpc.VTXO` values whose ancestry
paths carry real MuSig2 signatures over real tree nodes, so tests that gate on
expiry height, commitment depth, or ancestry verification exercise the
production verification path instead of a stub.

## Key Types

- `Round(t, value, script, delay, height, tag)` — Returns a signed
  round-direct inventory entry plus its commitment transaction. The `tag` byte
  seeds the deterministic cosigner keys and the commitment input hash, so
  independent fixtures get distinct but reproducible commitment IDs.
- `Merge(t, sameBatch)` — Builds a signed transaction consuming two round
  leaves and returns the resulting VTXO with both ancestry paths attached. With
  `sameBatch == true` the two parents share one confirmed commitment (distinct
  rooted paths within a single batch); otherwise the parents expire at
  different heights.

There are no exported types — the package exports only these two constructors.

## Relationships

- **Depends on**: `arkrpc` (`VTXO`, `AncestryPath`, `AncestryPathFromTree`,
  `AncestryPathToTree`), `lib/tree` (`Node`, `Tree`, `ComputeFinalKey`,
  `VerifySigned`), `lib/arkscript` (unilateral CSV timeout leaf, anchor
  script), `lib/tx/psbtutil` (PSBT serialization), and `musig2` for cosigner
  signing.
- **Depended on by** (tests only): `vtxo` (`expiry_target_test.go`,
  `incoming_ancestry_resolver_test.go`), `oor`
  (`incoming_metadata_query_test.go`, `receive_limits_test.go`), `waved`
  (`wallet_recovery_descriptor_test.go`,
  `wallet_recovery_oor_family_test.go`, `incoming_metadata_test.go`).
- **Sends** / **Receives**: none. This is a synchronous fixture builder with no
  actor involvement.

## Invariants

- **Fixtures are signed and self-verifying.** `Round` asserts
  `tree.Tree.VerifySigned()` before returning, so a fixture that would not pass
  production ancestry verification fails the test at construction time rather
  than producing a misleading downstream result.
- **`tag` must be unique per fixture within a test.** Both cosigner keys and
  the commitment input hash derive from it; reusing a tag collapses two
  fixtures onto the same commitment ID. `Merge` reserves tags `10` and `11`
  for its two parents.
- **Production-only code must never import this package.** It takes a
  `testing.TB` and calls `require`, so importing it outside `_test.go` pulls
  the testing framework into a production binary.
- **`internal/` scoping is deliberate.** The fixture encodes assumptions about
  ancestry encoding that are not a stable API for module consumers.

## Deep Docs

- [`internal/CLAUDE.md`](../CLAUDE.md) — Internal helper package map
- [`lib/tree/CLAUDE.md`](../../lib/tree/CLAUDE.md) — Tree node and signing
  model the fixture builds against
- [`docs/testing-guide.md`](../../docs/testing-guide.md) — Test approaches and
  coverage targets
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map
