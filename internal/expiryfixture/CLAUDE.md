# internal/expiryfixture

## Purpose

Test-only helper that builds signed VTXO ancestry for acceptance-boundary
tests. Several packages need a realistic inventory entry — a VTXO plus the
chain of transactions that proves where it came from — in order to exercise
expiry and ancestry logic. Constructing that by hand in each test is verbose
and easy to get subtly wrong, so this package centralizes it.

The fixtures are deliberately *not* produced by running the real state
machines: they are sufficient to test the inventory proof graph without
invoking the OOR state machine or a live round.

## Key Types

The package exposes functions rather than types:

- `Round(t, value, script, delay, height, tag)` — returns a signed
  round-direct inventory entry and its commitment transaction. The `tag` byte
  makes independent fixtures use distinct, reproducible commitment IDs.
- `Merge(t, sameBatch)` — builds a signed transaction consuming two round
  leaves. With `sameBatch` true the fixture exercises distinct rooted paths
  within one confirmed commitment; otherwise the two parents expire at
  different heights.

## Relationships

- **Depends on**: `arkrpc` (the `VTXO` wire type the fixtures return),
  `lib/arkscript` (VTXO policy script templates), `lib/tree` (leaf and
  commitment construction), `lib/tx/psbtutil` (transaction assembly).
- **Depended on by** (test files only): `vtxo`
  (`incoming_ancestry_resolver_test.go`, `expiry_target_test.go`), `oor`
  (`incoming_metadata_query_test.go`, `receive_limits_test.go`), `waved`
  (`wallet_recovery_descriptor_test.go`, `wallet_recovery_oor_family_test.go`,
  `incoming_metadata_test.go`).
- **Sends** / **Receives**: no actor messages.

## Invariants

- Test-only. Nothing under a non-test build path may import this package; it
  takes a `testing.TB` and fails the test directly on construction errors.
- Fixtures must stay deterministic. The `tag` parameter exists so that two
  fixtures in one test get different commitment IDs without introducing
  randomness — tests assert on those IDs.
- The returned ancestry is signed and internally consistent, but it is not
  produced by the production round or OOR paths. Do not use these fixtures to
  assert that those paths *produce* a given shape; use them to assert how
  consumers *interpret* a given shape.

## Deep Docs

- [vtxo/CLAUDE.md](../../vtxo/CLAUDE.md) — VTXO inventory and expiry.
- [oor/CLAUDE.md](../../oor/CLAUDE.md) — Out-of-round transfer sessions.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
