# itest

## Purpose

Real-daemon integration tests exercising end-to-end Ark flows with in-process
operator and client daemon processes. Tests cover boarding, out-of-round (OOR)
transfers, refresh lifecycle, directed sends, and daemon restart resilience.

## Key Test Categories

- **Boarding** (`boarding_test.go`) — Single/multi-client boarding
  registration, shared rounds, subsequent rounds, restart-after-broadcast,
  restart-after-input-sig, seal-triggered batch creation, board with no
  confirmed inputs.
- **OOR** (`oor_test.go`) — Alice-to-Bob transfer, bidirectional transfers,
  multi-input transfers, chained transfers, resume across client restart,
  offline recipient event visibility (exercised on both LND and lwwallet
  client backends now that the indexer test client is backend-agnostic and
  the harness supports prebuilt mailbox query requests).
- **Send VTXO** (`send_vtxo_test.go`) — Directed VTXO sends with dry-run
  preview, `Output_Pubkey` directed sends, zero-amount validation.
- **Directed Send** (`send_test.go`) — Integration coverage for
  in-round directed sends, including non-participant recipient
  materialization via the `VTXOEventPublisher` -> indexer flow and
  `Origin=VTXO_ORIGIN_IN_ROUND`/absolute batch expiry propagation.
- **Refresh** (`refresh_test.go`) — Single-VTXO lifecycle, dry-run preview,
  all-selection with queued live outpoints, and refresh of VTXOs received
  via OOR (exercises the atomic finalize+materialize path end-to-end).
- **Sweep** (`sweep_test.go`) — Expired batch sweep integration test
  validating the full batchsweeper lifecycle end-to-end.
- **vHTLC OOR** (`vhtlc_oor_test.go`) — End-to-end vHTLC policy coverage:
  Alice boards a standard VTXO, sends it out-of-round to a vHTLC policy
  template, Bob discovers the indexed vHTLC output via
  `GetIndexedVTXOByPkScript`, and claims it through `SendOOR` with
  `custom_inputs`, sweeping the value into a fresh VTXO.
- **Helpers** (`helpers_test.go`) — Shared test utilities: boarding flows,
  balance assertions, round waiting, client setup, RPC validation harness.
  Includes `waitForIndexedVTXOByPkScript` for polling the indexer until a
  VTXO with a given pkScript reaches a target lifecycle status.

## Relationships

- **Depends on**: `harness` (test environment), `adminrpc` (operator admin
  queries), `client/daemonrpc` (client RPCs), `client/arkrpc` (Ark RPCs),
  `client/harness` (client harness options).
- **Depended on by**: none (top-level test package).

## Invariants

- Build tag `itest` gates all files; these tests are not included in normal
  `go test` runs.
- Tests use `harness.NewArkHarness` for environment setup and must not
  manage infrastructure directly.
- Each test function is self-contained with its own harness instance.
- Run via `make itest icase=<TestName>`.
- Tests exercising non-LND client wallet backends must not assume access to
  `daemon.LND.Client.WalletKit`; use the backend-agnostic harness APIs.

## Deep Docs

- [docs/testing-guide.md](../docs/testing-guide.md) — Test approaches and coverage targets.
- [docs/layered_testing_guide.md](../docs/layered_testing_guide.md) — Test layering strategy.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
