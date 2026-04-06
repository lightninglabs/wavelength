# itest

## Purpose

Real-daemon integration tests exercising end-to-end Ark flows with in-process
operator and client daemon processes. Tests cover boarding, out-of-round (OOR)
transfers, refresh lifecycle, and daemon restart resilience.

## Key Test Categories

- **Boarding** (`boarding_test.go`) — Single/multi-client boarding registration,
  shared rounds, subsequent rounds, restart-after-broadcast, restart-after-input-sig,
  seal-triggered batch creation, board with no confirmed inputs.
- **OOR** (`oor_test.go`) — Alice-to-Bob transfer, bidirectional transfers,
  multi-input transfers, chained transfers, resume across client restart,
  offline recipient event visibility. Daemon-level transfer coverage.
- **Send VTXO** (`send_vtxo_test.go`) — Directed VTXO sends with dry-run
  preview, `Output_Pubkey` directed sends, zero-amount validation.
- **Refresh** (`refresh_test.go`) — Single-VTXO lifecycle, dry-run preview,
  all-selection with queued live outpoints.
- **Sweep** (`sweep_test.go`) — Expired batch sweep integration test validating
  the full batchsweeper lifecycle end-to-end.
- **Helpers** (`helpers_test.go`) — Shared test utilities: boarding flows,
  balance assertions, round waiting, client setup, RPC validation harness.

## Relationships

- **Depends on**: `harness` (test environment), `adminrpc` (operator admin
  queries), `client/daemonrpc` (client RPCs), `client/arkrpc` (Ark RPCs),
  `client/harness` (client harness options).
- **Depended on by**: none (top-level test package).

## Invariants

- Build tag `itest` gates all files; these tests are not included in normal
  `go test` runs.
- Tests use `harness.NewArkHarness` for environment setup and must not manage
  infrastructure directly.
- Each test function is self-contained with its own harness instance.
- Run via `make itest icase=<TestName>`.

## Deep Docs

- [docs/testing-guide.md](../docs/testing-guide.md) — Test approaches and coverage targets.
- [docs/layered_testing_guide.md](../docs/layered_testing_guide.md) — Test layering strategy.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
