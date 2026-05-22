# itest

## Purpose

Real-daemon integration tests exercising end-to-end Ark flows with in-process
operator + client daemon processes. Boarding, OOR, refresh, directed sends,
unroll, sweep, fees, and daemon restart resilience.

Build tag `itest`; not in normal `go test`. Run via `make itest icase=<TestName>`.

## Test Categories

| File | Coverage |
|------|----------|
| `boarding_test.go` | Single/multi-client boarding, shared rounds, subsequent rounds, restart-after-broadcast, restart-after-input-sig, seal-triggered batch, board with no confirmed inputs. |
| `oor_test.go` | Alice→Bob, bidirectional, multi-input, chained, resume across client restart, offline-recipient visibility (both LND and lwwallet backends via backend-agnostic indexer client + prebuilt mailbox query support). |
| `send_vtxo_test.go` | Directed sends with dry-run preview, `Output_Pubkey` sends, zero-amount validation. |
| `send_test.go` | In-round directed sends; non-participant materialization via `VTXOEventPublisher` → indexer; `Origin=VTXO_ORIGIN_IN_ROUND` + absolute batch-expiry propagation. |
| `refresh_test.go` | Single-VTXO lifecycle, dry-run preview, all-selection with queued live outpoints, refresh of VTXOs received via OOR (atomic finalize+materialize end-to-end). |
| `sweep_test.go` | Expired batch sweep — full batchsweeper lifecycle. |
| `boarding_sweep_test.go` | Manual expired-boarding sweep: funds boarding UTXO, skips Board, mines past reduced `BoardingExitDelay` (16 blocks), previews and broadcasts `SweepBoardingUTXOs` to external operator-LND taproot, asserts `ListBoardingSweeps`, `BoardingPendingSweepSat`/`BoardingSweptSat` breakdown, operator-LND receipt, `boarding_sweep_fee_paid` in `GetFeeHistory`. |
| `vhtlc_oor_test.go` | vHTLC end-to-end: Alice boards, OOR-sends to vHTLC template, Bob discovers via `GetIndexedVTXOByPkScript` and claims via `SendOOR` with `custom_inputs`, sweeping to a fresh VTXO. |
| `unroll_test.go` | Unilateral exit: manual trigger, dedup, VTXO transitions, recovery chain materialization, CSV wait, sweep, completion. Reduced `VTXOExitDelay` (10 blocks). `newUnrollHarness` builds the env + funds operator LND. |
| `helpers_test.go` | `waitForIndexedVTXOByPkScript`, `confirmedWalletUTXOValues`, `waitForNewConfirmedWalletUTXOWithMaxValue`, boarding/balance/round helpers. |
| `fees_admin_rpc_test.go` | Hot-reload via `UpdateFeeSchedule`; `GetTreasuryStatus`; `ListFeeEvents`. |
| `fees_validation_test.go` | Dynamic fee validation: below-minimum rejection when `MinViablePolicy=reject`; quote enforcement. |
| `fees_hotreload_test.go` | End-to-end hot-reload + restart persistence (loaded from `fee_schedule_history`). |
| `fees_congestion_test.go` | Congestion activation past utilization threshold. |
| `fees_treasury_rehydration_test.go` | `TreasuryTracker` rehydrate from ledger after restart. |
| `fees_classifier_test.go` | UTXO diff classifier: round/sweep-attributable movements not double-booked as external_*. |
| `fees_disabled_regression_test.go` | Zero schedule doesn't break flows. |
| `fees_helpers_test.go` | `takeLedgerSnapshot`, `assertLedgerDelta`, fee assertion helpers. |

## Relationships

- **Depends on**: `harness`, `adminrpc`, `client/daemonrpc`,
  `client/arkrpc`, `client/harness`.

## Invariants

- Use `harness.NewArkHarness`; don't manage infrastructure directly.
- Each test is self-contained with its own harness instance.
- Tests against non-LND client wallet backends must not reach
  `daemon.LND.Client.WalletKit` — use backend-agnostic harness APIs.

## Deep Docs

- [docs/testing-guide.md](../docs/testing-guide.md) — Approaches + coverage.
- [docs/layered_testing_guide.md](../docs/layered_testing_guide.md) — Test
  layering strategy.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide map.
