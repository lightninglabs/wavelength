# internal/expiryfixture

## Purpose

Builds signed VTXO ancestry for acceptance-boundary tests. The authenticated
incoming-expiry path refuses to trust the scalars on an indexer push and
instead re-derives batch expiry from a locally confirmed commitment
transaction, so its tests need *real* signed commitment trees rather than hand
stuffed structs. This package produces those fixtures once, deterministically,
so every package testing that boundary asserts against the same graph.

## Key Types

- `Round(t, value, script, delay, height, tag) (*arkrpc.VTXO, *wire.MsgTx)` —
  A signed round-direct inventory entry and its commitment transaction. `tag`
  seeds the owner and operator keys, so independent fixtures get distinct but
  reproducible commitment IDs.
- `Merge(t, sameBatch bool) (*arkrpc.VTXO, []*wire.MsgTx)` — A signed
  transaction consuming two round leaves. `sameBatch=true` exercises two
  distinct rooted paths inside one confirmed commitment; `false` gives parents
  that expire at different heights. The returned package is sufficient to
  exercise the inventory proof graph without driving the OOR state machine.

## Relationships

- **Depends on**: `arkrpc` (`VTXO`, ancestry package protos), `lib/arkscript`
  (`UnilateralCSVTimeoutTapLeaf`, policy templates), `lib/tree`
  (`ComputeFinalKey`, tree construction), `lib/tx/psbtutil` (PSBT signing
  helpers), `btcec`/`musig2`/`psbt`/`txscript`/`wire` (btcd),
  `testify/require`.
- **Depended on by**: test files only — `vtxo` (`expiry_target_test.go`,
  `incoming_ancestry_resolver_test.go`), `oor`
  (`incoming_metadata_query_test.go`, `receive_limits_test.go`), `waved`
  (`incoming_metadata_test.go`, `wallet_recovery_descriptor_test.go`,
  `wallet_recovery_oor_family_test.go`).

## Invariants

- **Fixtures must stay deterministic.** Keys derive from the caller's `tag`
  byte and commitment IDs follow from them, so a golden expiry height or
  commitment txid asserted in one package stays stable. Changing the key
  derivation changes every dependent test's expected values at once.
- **The ancestry must actually verify.** These fixtures exist specifically to
  pass `vtxo.IndexedAncestryFromRPC` and `AuthenticateBatchExpiry`, which
  recompute the tree-root output script and byte-match it against the returned
  commitment transaction. A fixture that skips signing or emits a mismatched
  sweep leaf silently converts an acceptance test into a rejection test.
- **Test-only, and internal on purpose.** Every function takes `testing.TB` and
  calls `require`. It lives under `internal/` so it can never be imported by a
  downstream module, and it must never be reachable from production code.

## Deep Docs

- [vtxo/CLAUDE.md](../../vtxo/CLAUDE.md) — Authenticated incoming expiry and
  the ancestry acceptance boundary.
- [docs/testing-guide.md](../../docs/testing-guide.md) — Test approaches and
  coverage targets.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
