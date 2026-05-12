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
- **Boarding sweep** (`boarding_sweep_test.go`) — Manual expired-boarding
  sweep end-to-end: funds a boarding UTXO, skips Board, mines past a
  reduced `BoardingExitDelay` (16 blocks), previews and broadcasts
  `SweepBoardingUTXOs` to an external operator-LND taproot address,
  then asserts `ListBoardingSweeps` confirmed transitions, the
  `BoardingPendingSweepSat` / `BoardingSweptSat` `GetBalance` breakdown,
  operator-LND receipt of the swept output, and the new
  `boarding_sweep_fee_paid` entry in `GetFeeHistory`.
- **vHTLC OOR** (`vhtlc_oor_test.go`) — End-to-end vHTLC policy coverage:
  Alice boards a standard VTXO, sends it out-of-round to a vHTLC policy
  template, Bob discovers the indexed vHTLC output via
  `GetIndexedVTXOByPkScript`, and claims it through `SendOOR` with
  `custom_inputs`, sweeping the value into a fresh VTXO.
- **Unroll** (`unroll_test.go`) — Unilateral exit integration tests: manual
  trigger, dedup, VTXO status transitions, recovery chain materialization,
  CSV wait, sweep, and completion. Uses a reduced `VTXOExitDelay` (10 blocks)
  to keep block-mining time reasonable. Harness helper `newUnrollHarness`
  creates the short-delay environment and funds the operator LND wallet.
- **Helpers** (`helpers_test.go`) — Shared test utilities: boarding flows,
  balance assertions, round waiting, client setup, RPC validation harness.
  Includes `waitForIndexedVTXOByPkScript` for polling the indexer until a
  VTXO with a given pkScript reaches a target lifecycle status.
  `confirmedWalletUTXOValues` and `waitForNewConfirmedWalletUTXOWithMaxValue`
  are new helpers for unroll tests that detect swept VTXO outputs in the
  client's wallet after CSV expiry.
- **Fees — Admin RPC** (`fees_admin_rpc_test.go`) — Hot-reload of fee schedule
  via `UpdateFeeSchedule` admin RPC, treasury status tracking via
  `GetTreasuryStatus`, and `ListFeeEvents` ledger event history assertions.
- **Fees — Validation** (`fees_validation_test.go`) — Dynamic fee validation
  in round join requests: boarding below minimum viable threshold is rejected
  when `MinViablePolicy=reject`, and fee quotes are enforced against the
  computed schedule.
- **Fees — Hot Reload** (`fees_hotreload_test.go`) — End-to-end hot-reload
  test: updates fee schedule at runtime, restarts arkd, verifies the new
  schedule persists (loaded from `fee_schedule_history` on restart).
- **Fees — Congestion** (`fees_congestion_test.go`) — Congestion pricing
  activation: when treasury utilization exceeds the threshold, the fee
  spread activates and quotes change accordingly.
- **Fees — Treasury Rehydration** (`fees_treasury_rehydration_test.go`) —
  Verifies that the in-memory `TreasuryTracker` is correctly rehydrated from
  the persisted ledger after a daemon restart.
- **Fees — Classifier** (`fees_classifier_test.go`) — UTXO diff classifier
  groundwork: verifies round-attributable and sweep-attributable wallet
  movements are not double-booked as external_* ledger events.
- **Fees — Disabled Regression** (`fees_disabled_regression_test.go`) —
  Regression coverage confirming that disabling fees (zero schedule) does
  not break existing Ark flows.
- **Fees — Helpers** (`fees_helpers_test.go`) — Shared fee test utilities:
  `takeLedgerSnapshot`, `assertLedgerDelta`, fee schedule assertion helpers.

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
