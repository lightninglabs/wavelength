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
  GetUnrollStatus. The admin-shape methods (GenSeed/InitWallet/
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
    `ListVTXOsRequest`, `ListTransactionsRequest`, `NewAddressRequest`,
    `GetBalanceRequest`, `GetInfoRequest`
  - → swapclientrpc (in-process via SwapService): `StartPayRequest`,
    `StartReceiveRequest`, `ListSwapsRequest`, `SubscribeSwapsRequest`
- **Receives**:
  - ← API: `walletrpc.{Create,Unlock,Send,Recv,List,Balance,Deposit,
    Status,Exit,ExitStatus,SubscribeWallet}Request`

## Invariants

- Admin handlers (`Create`/`Unlock`/`Exit`/`ExitStatus`) are
  admin-shape: they reach the daemon-side admin RPC via the injected
  RPCServer and DO NOT depend on Runtime, router, recv, or history.
  Create and Unlock must work before the swap subsystem is live.
- Background goroutines (monitor loop, deadline watcher, resume sweep)
  are anchored to the daemon rootCtx, NEVER to RPC-call contexts. An
  RPC client disconnect cannot cancel in-flight work.
- `WalletEntry.id` is the stable canonical id for SEND-invoice and RECV
  (Lightning payment_hash) across the entire pending → terminal
  lifecycle. EXIT and DEPOSIT rows do NOT yet share an id between
  pending and confirmed in v1; see `doc.go` for the limitation.
- Onchain SEND has whole-VTXO sweep semantics; the recipient may
  receive more than amt_sat. `SendResponse.actual_amount_sat` carries
  the true outflow and SHOULD be echoed back before the send is
  treated as confirmed.
- `ListView` defaults to Activity. The Activity view is the only view
  that honors `pending_only` and `kinds`; those filters are ignored
  for VTXOs and Onchain views.
- VTXOs view filters out terminal internal states (FORFEITED, SPENT,
  FAILED) so the wallet view stays focused on VTXOs the user can act
  on.
- The runtime's deadline overlay elevates stuck PENDING entries to
  FAILED with `failure_reason="timed_out"` BEFORE filtering, so a
  stuck row appears as FAILED in the wallet view even when the caller
  asked for `pending_only=false`.

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
