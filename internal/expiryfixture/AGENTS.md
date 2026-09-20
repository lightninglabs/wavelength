# internal/expiryfixture

## Purpose

Builds signed VTXO ancestry for acceptance-boundary tests. Produces real
musig2-signed tree nodes and `arkrpc.AncestryPath` payloads from deterministic
keys, so tests that exercise inventory-proof validation, expiry targeting, and
incoming-metadata checks can work against ancestry that actually verifies —
without standing up a round or driving the OOR state machine.

## Key Types

- `Round(t, value, script, delay, height, tag)` — Returns a signed round-direct
  `arkrpc.VTXO` and the commitment transaction it descends from. The `tag` byte
  seeds the fixture's keys and the commitment's input hash, so independent
  fixtures in one test get distinct, reproducible commitment ids.
- `Merge(t, sameBatch)` — Returns a VTXO spending two round leaves, plus the
  commitments backing them. `sameBatch=true` puts both parents under one
  confirmed commitment (distinct rooted paths, one expiry); `false` leaves them
  in separate commitments that expire at different heights.

## Relationships

- **Depends on**: `arkrpc` (`VTXO`, `AncestryPath`, `AncestryPathFromTree` /
  `AncestryPathToTree`), `lib/tree` (`Node`, `Tree`, `ComputeFinalKey`,
  `VerifySigned`), `lib/arkscript` (`UnilateralCSVTimeoutTapLeaf`,
  `AnchorPkScript`), `lib/tx/psbtutil` (serializing the merge PSBT), `btcd`
  musig2 / txscript, `testify/require`.
- **Depended on by**: test files only — `vtxo` (`expiry_target_test.go`,
  `incoming_ancestry_resolver_test.go`), `oor`
  (`incoming_metadata_query_test.go`, `receive_limits_test.go`), `waved`
  (`incoming_metadata_test.go`, `wallet_recovery_descriptor_test.go`,
  `wallet_recovery_oor_family_test.go`).

## Invariants

- Fixtures are **really signed**, not stubbed: `Round` runs a two-party musig2
  session over the node sighash and asserts `Tree.VerifySigned()` before
  returning. A test that accepts a fixture is therefore testing signature-valid
  ancestry, and changes here must keep that assertion passing.
- Keys are derived from the `tag` byte (`{tag, 1}` owner, `{tag, 2}` operator),
  so the whole fixture is deterministic. Two fixtures sharing a tag share keys
  and commitment ids — pass distinct tags when a test needs them to be
  independent.
- Every entry-point takes `testing.TB` and calls `t.Helper()`; failures are
  raised via `require`, so callers get no error to handle. This package must
  stay test-only and must never be imported from production code.

## Deep Docs

- [internal/CLAUDE.md](../CLAUDE.md) — Parent internal package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
