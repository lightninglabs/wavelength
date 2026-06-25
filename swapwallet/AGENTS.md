# swapwallet

## Purpose

Daemon-side implementation of the `walletdkrpc.WalletService` gRPC
subserver. It composes the swap subsystem (`swapclientserver`), the
cooperative-leave RPC, the daemon's wallet/admin surface, the boarding
ledger, and the unilateral-exit registry into one flat user-facing API:
the seven wallet verbs (Create, Unlock, Send, Recv, List, Balance,
Exit) plus the supporting Deposit / Status / SubscribeWallet methods.

The whole package lives behind `//go:build walletdkrpc && swapruntime` so
default builds avoid the swap executor's dependency graph.

## Key Types

- `Service` — gRPC handler implementing `walletdkrpc.WalletServiceServer`.
  Thin facade: each method dispatches to `router`, `receiver`,
  `history`, or admin proxy helpers; no business logic lives here.
- `Runtime` — Owns the in-process swap lifecycle: synchronous
  resume-on-startup, deadline watcher (overlays stuck entries as
  FAILED), monitor loop (fans normalized updates to subscribers).
  Anchored to the daemon root context so an RPC client disconnect can
  never cancel in-flight work.
- `Deps` — Composition struct: `SwapBackend` (in-Go swap runtime),
  `SwapService` (gRPC-shaped swap subserver handle), `RPCServer`
  (narrow daemonrpc contract), `ChainParams` (Bitcoin network — used to
  validate BOLT-11 invoice decoding in `PrepareSend` so a cross-network
  invoice is rejected before a send intent is issued), plus wallet-level
  deadline, list-limit, and subscribe-buffer knobs.
- `RPCServer` interface — Narrow contract over `*darepod.RPCServer`
  covering every daemonrpc method swapwallet composes against:
  LeaveVTXOs, SendOnChain, ListVTXOs, ListTransactions, NewAddress, GetInfo,
  EstimateFee, GetBalance, GenSeed, InitWallet, UnlockWallet, Unroll,
  GetUnrollStatus, GetExitPlan, SweepWallet, JoinNextRound,
  NewWalletAddress, ListWalletUnspent. The admin-shape methods
  (GenSeed/InitWallet/UnlockWallet/Unroll/GetUnrollStatus) are
  reachable BEFORE the swap runtime is live.
- `WalletEntry` (re-exported from walletdkrpc) — Flat row type the entire
  history/streaming surface returns. Every internal correlator
  (session_id, round_id, settlement_type, mailbox subtype) is dropped
  before responding.
- `ListView` (re-exported from walletdkrpc) — Selects between Activity
  (merged WalletEntry stream), VTXOs (live inventory), and Onchain
  (boarding + sweep) views. Default is Activity.

## Relationships

- **Depends on**:
  - `rpc/walletdkrpc` (generated gRPC stubs and request/response shapes)
  - `daemonrpc` (admin RPCs proxied by Create/Unlock/Exit and the
    backends consumed for LeaveVTXOs, ListVTXOs, ListTransactions,
    GetBalance, NewAddress)
  - `rpc/swapclientrpc` (swap-subsystem gRPC shape; ListSwaps,
    StartPay, StartReceive)
  - `swapclientserver` (typed `Backend` handle and runtime resume)
  - `darepod` (`SwapBackend` interface)
  - `ledger` (account name constants for OOR ledger projection)
  - `btclog/v2` (subsystem logger)
- **Depended on by**:
  - `cmd/darepod` (`walletdkrpc.go` registers the subserver behind the
    walletdkrpc build tag)
  - `sdk/walletdk` (gomobile-friendly SDK wraps the same gRPC service)
- **Sends**:
  - → daemonrpc (in-process via RPCServer):
    `InitWalletRequest`, `UnlockWalletRequest`, `GenSeedRequest`,
    `UnrollRequest`, `GetUnrollStatusRequest`, `LeaveVTXOsRequest`,
    `SendOnChainRequest`, `JoinNextRoundRequest`, `ListVTXOsRequest`,
    `ListTransactionsRequest`, `NewAddressRequest`, `GetBalanceRequest`,
    `GetInfoRequest`
  - → swapclientrpc (in-process via SwapService): `QuotePayRequest`,
    `StartPayRequest`, `StartReceiveRequest`, `ListSwapsRequest`,
    `SubscribeSwapsRequest`
- **Receives**:
  - ← API: `walletdkrpc.{Create,Unlock,Send,Recv,List,Balance,Deposit,
    Status,Exit,ExitStatus,SubscribeWallet}Request`

## Invariants

- Admin handlers (`Create`/`Unlock`/`Exit`/`ExitStatus`) are
  admin-shape: they reach daemonrpc via the injected `RPCServer` and
  DO NOT depend on `Runtime`, router, recv, or history. Create and
  Unlock must work before the swap subsystem is live.
- Background goroutines (monitor loop, deadline watcher, resume
  sweep) are anchored to the daemon root context, NEVER to RPC-call
  contexts. An RPC client disconnect cannot cancel in-flight work.
- `WalletEntry.id` is the stable canonical id for SEND-invoice and
  RECV (Lightning payment_hash) across the entire pending → terminal
  lifecycle. EXIT and DEPOSIT rows do not yet share an id between
  pending and confirmed in v1; see `doc.go`.
- **Cooperative leave rows complete** via `decorateCooperativeLeaveEntry`
  once the source VTXO appears in the FORFEITED terminal state in
  `ListVTXOs`. This is best-effort in v1: a daemon restart drops the
  runtime-local pending EXIT row and the original counterparty/note cannot
  be recovered until the daemon persists a leave job that links queued
  outpoints to the commitment tx.
- **`PrepareSend` for invoices calls `QuotePay` first**: the router calls
  `SwapService.QuotePay` before storing the intent so the preview reflects
  the server-side fee, rail (Lightning vs in-Ark), and exact outflow. If the
  swap server is older and returns `Unimplemented` / `NotFound`, the router
  falls back to a local-only preview with `SEND_QUOTE_STATUS_LOCAL_ONLY`
  and a warning; the subsequent `Send` still resolves the real fee.
- Onchain SEND is routed through `RPCServer.SendOnChain`. Two modes:
  **sweep-all** (drains all live VTXOs, no change, leave output absorbs fee)
  and **bounded** (selects VTXOs to cover `TargetAmountSat` + headroom,
  produces a change VTXO under the #270 handshake). `SendOnChain` registers
  the intent atomically (`TriggerRegistration` set inside the daemon
  handler); there is no explicit `JoinNextRound` call after it.
- `SendResponse.actual_amount_sat` carries the true outflow for sweep-all
  sends (zero `amountSat` in the intent) and the exact requested amount for
  bounded sends.
- **Recv pre-flight**: `checkReceiveLimits` enforces `MaxVtxoAmount` (
  per-VTXO cap) and `MaxUserBalance` (total balance cap) from `GetInfo`
  before a swap session and invoice are created. Both checks fail open — a
  transient error skips the affected check rather than blocking the receive.
  Balance accounting sums live VTXOs plus all boarding buckets
  (confirmed + unconfirmed + adopted). The check never blocks when the
  operator is unreachable.
- `ListView` defaults to Activity. Only Activity honors
  `pending_only` and `kinds`; those filters are ignored for VTXOs
  and Onchain.
- VTXOs view filters out terminal internal states (FORFEITED, SPENT,
  FAILED) so the wallet view stays focused on actionable VTXOs.
- The runtime's deadline overlay elevates stuck PENDING entries to
  FAILED with `failure_reason="timed_out"` BEFORE filtering, so a
  stuck row appears as FAILED even when the caller asks for
  `pending_only=false`.
- **DEPOSIT rows backed by the `wallet_utxo_created` ledger event**
  mirror the ledger confirmation status. Confirmed on-chain boarding
  deposits surface as `ENTRY_STATUS_COMPLETE`, while unconfirmed
  boarding funds are represented by the synthetic
  `boarding-unconfirmed` pending row from `GetBalance`.
- **`Balance` projection** maps daemonrpc fields onto the walletdkrpc
  shape: `confirmed_sat` is VTXO-only (`vtxo_balance_sat`),
  `pending_in_sat` sums `boarding_confirmed_sat +
  boarding_unconfirmed_sat + boarding_adopted_sat`, and
  `pending_out_sat` mirrors `boarding_pending_sweep_sat`.
  Confirmed-but-not-yet-boarded UTXOs must NOT inflate
  `confirmed_sat` (issue #502), and adopted-but-not-yet-live VTXOs
  must stay pending inbound until commitment confirmation (issue #542).

## Deep Docs

- [docs/walletdkrpc_build.md](../docs/walletdkrpc_build.md) — Build modes,
  make targets, what the walletdkrpc tag enables.
- [docs/walletdk_integration.md](../docs/walletdk_integration.md) —
  How `sdk/walletdk` wraps the same gRPC service for embedded hosts.
- [docs/swap_background_execution.md](../docs/swap_background_execution.md)
  — Daemon-side swap lifecycle the runtime composes over.
- [rpc/walletdkrpc/CLAUDE.md](../rpc/walletdkrpc/CLAUDE.md) — Proto schema
  and per-message invariants.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
