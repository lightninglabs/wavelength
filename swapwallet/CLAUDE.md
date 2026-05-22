# swapwallet

## Purpose

Daemon-side implementation of the `walletrpc.WalletService` gRPC
subserver. It composes the swap subsystem (`swapclientserver`), the
cooperative-leave RPC, the daemon's wallet/admin surface, the boarding
ledger, and the unilateral-exit registry into one flat user-facing API:
the seven wallet verbs (Create, Unlock, Send, Recv, List, Balance,
Exit) plus the supporting Deposit / Status / SubscribeWallet methods.

The whole package lives behind `//go:build walletrpc && swapruntime` so
default builds avoid the swap executor's dependency graph.

## Key Types

- `Service` — gRPC handler implementing `walletrpc.WalletServiceServer`.
  Thin facade: each method dispatches to `router`, `receiver`,
  `history`, or admin proxy helpers; no business logic lives here.
- `Runtime` — Owns the in-process swap lifecycle: synchronous
  resume-on-startup, deadline watcher (overlays stuck entries as
  FAILED), monitor loop (fans normalized updates to subscribers).
  Anchored to the daemon root context so an RPC client disconnect can
  never cancel in-flight work.
- `Deps` — Composition struct: `SwapBackend` (in-Go swap runtime),
  `SwapService` (gRPC-shaped swap subserver handle), `RPCServer`
  (narrow daemonrpc contract), plus wallet-level deadline, list-limit,
  and subscribe-buffer knobs.
- `RPCServer` interface — Narrow contract over `*darepod.RPCServer`
  covering every daemonrpc method swapwallet composes against:
  LeaveVTXOs, ListVTXOs, ListTransactions, NewAddress, GetInfo,
  GetBalance, GenSeed, InitWallet, UnlockWallet, Unroll,
  GetUnrollStatus, JoinNextRound. The admin-shape methods (GenSeed/InitWallet/
  UnlockWallet/Unroll/GetUnrollStatus) are reachable BEFORE the swap
  runtime is live.
- `WalletEntry` (re-exported from walletrpc) — Flat row type the entire
  history/streaming surface returns. Every internal correlator
  (session_id, round_id, settlement_type, mailbox subtype) is dropped
  before responding.
- `ListView` (re-exported from walletrpc) — Selects between Activity
  (merged WalletEntry stream), VTXOs (live inventory), and Onchain
  (boarding + sweep) views. Default is Activity.

## Relationships

- **Depends on**:
  - `rpc/walletrpc` (generated gRPC stubs and request/response shapes)
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
  - `cmd/darepod` (`walletrpc.go` registers the subserver behind the
    walletrpc build tag)
  - `sdk/walletdk` (gomobile-friendly SDK wraps the same gRPC service)
- **Sends**:
  - → daemonrpc (in-process via RPCServer):
    `InitWalletRequest`, `UnlockWalletRequest`, `GenSeedRequest`,
    `UnrollRequest`, `GetUnrollStatusRequest`, `LeaveVTXOsRequest`,
    `JoinNextRoundRequest`, `ListVTXOsRequest`,
    `ListTransactionsRequest`, `NewAddressRequest`, `GetBalanceRequest`,
    `GetInfoRequest`
  - → swapclientrpc (in-process via SwapService): `StartPayRequest`,
    `StartReceiveRequest`, `ListSwapsRequest`, `SubscribeSwapsRequest`
- **Receives**:
  - ← API: `walletrpc.{Create,Unlock,Send,Recv,List,Balance,Deposit,
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
- Onchain SEND has whole-VTXO sweep semantics; the recipient may
  receive more than `amt_sat`. `SendResponse.actual_amount_sat`
  carries the true outflow and SHOULD be echoed back before the send
  is treated as confirmed.
- **Onchain SEND is a one-shot**: after `LeaveVTXOs` returns
  successfully the router immediately calls `JoinNextRound` so the
  queued leave intent is committed to the next round without a
  separate CLI step. The raw `ark vtxos leave --no_join` path
  remains the batching seam for callers that want to combine
  multiple intents into one round; the top-level `send` verb is
  intentionally not exposed to that mode. If the implicit join
  fails, the error carries the explicit recovery hint (`ark rounds
  join`) so the leave intent — already queued in the round actor —
  is not stranded silently.
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
  are pinned to `ENTRY_STATUS_PENDING` even after chain confirmation
  (`statusForLedgerRow`), because a boarding UTXO landing on-chain
  is not yet a spendable VTXO. Promotion to COMPLETE waits for a
  follow-up `boarded-into-round` ledger event (issue #503).
- **`Balance` projection** maps daemonrpc fields onto the walletrpc
  shape: `confirmed_sat` is VTXO-only (`vtxo_balance_sat`),
  `pending_in_sat` sums `boarding_confirmed_sat +
  boarding_unconfirmed_sat`, and `pending_out_sat` mirrors
  `boarding_pending_sweep_sat`. Confirmed-but-not-yet-boarded UTXOs
  must NOT inflate `confirmed_sat` (issue #502).

## Deep Docs

- [docs/walletrpc_build.md](../docs/walletrpc_build.md) — Build modes,
  make targets, what the walletrpc tag enables.
- [docs/walletdk_integration.md](../docs/walletdk_integration.md) —
  How `sdk/walletdk` wraps the same gRPC service for embedded hosts.
- [docs/swap_background_execution.md](../docs/swap_background_execution.md)
  — Daemon-side swap lifecycle the runtime composes over.
- [rpc/walletrpc/CLAUDE.md](../rpc/walletrpc/CLAUDE.md) — Proto schema
  and per-message invariants.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
