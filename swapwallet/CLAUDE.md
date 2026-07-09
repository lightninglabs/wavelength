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
  FAILED), monitor loop (fans normalized updates to subscribers), and
  the canonical activity-log projector (`project`/`projectAndEmit`)
  that durably writes every emitted `WalletEntry` and stamps it with
  the store-assigned `event_seq` before fanning it out, so
  `SubscribeWallet` can hand callers a resumable cursor. Anchored to
  the daemon root context so an RPC client disconnect can never
  cancel in-flight work.
- `Deps` — Composition struct: `SwapBackend` (in-Go swap runtime),
  `SwapService` (gRPC-shaped swap subserver handle), `RPCServer`
  (narrow daemonrpc contract), `ChainParams` (Bitcoin network — used to
  validate BOLT-11 invoice decoding in `PrepareSend` so a cross-network
  invoice is rejected before a send intent is issued), plus wallet-level
  deadline, list-limit, and subscribe-buffer knobs.
- `RPCServer` interface — Narrow contract over `*darepod.RPCServer`
  covering every daemonrpc method swapwallet composes against:
  LeaveVTXOs, SendOnChain, ListVTXOs, ListTransactions, NewAddress, GetInfo,
  GetBalance, GenSeed, InitWallet, UnlockWallet, Unroll,
  GetUnrollStatus, JoinNextRound. The admin-shape methods (GenSeed/InitWallet/
  UnlockWallet/Unroll/GetUnrollStatus) are reachable BEFORE the swap
  runtime is live.
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
  - `db` (`ActivityProjection` DTO fed to the canonical activity-log
    projector wired in via `Deps.ActivityStore`)
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
  - → swapclientrpc (in-process via SwapService): `StartPayRequest`,
    `StartReceiveRequest`, `ListSwapsRequest`, `SubscribeSwapsRequest`
- **Receives**:
  - ← API: `walletdkrpc.{Create,Unlock,Send,Recv,List,Balance,Deposit,
    Status,Exit,ExitStatus,ExitSummary,SubscribeWallet}Request`

## Invariants

- Admin handlers (`Create`/`Unlock`/`Exit`/`ExitStatus`/`ExitSummary`)
  are admin-shape: they reach daemonrpc via the injected `RPCServer`
  and DO NOT depend on `Runtime`, router, recv, or history. Create and
  Unlock must work before the swap subsystem is live.
- Background goroutines (monitor loop, deadline watcher, resume
  sweep) are anchored to the daemon root context, NEVER to RPC-call
  contexts. An RPC client disconnect cannot cancel in-flight work.
- `projectMu` serializes every project-then-emit across all concurrent
  producers (monitor loop, reconciler, credit poll, deadline watcher,
  RPC handlers) so the `event_seq` a transition is assigned by the
  store and the live emit that carries it stay in the same order. A
  transition only reaches subscribers when it is durable (`seq > 0`);
  a change-suppressed no-op or a failed projection emits nothing.
  Without this lock a later-committed but lower-seq event could emit
  after a higher one, and a `SubscribeWallet` cursor would advance
  past it, silently dropping the update.
- A dispatched pure-Lightning invoice send (`sendInvoiceIntent`)
  eagerly calls `project` (not `projectAndEmit`) for its pending row
  immediately after `StartPay` accepts it, off the RPC context, so a
  caller polling `List`/`InspectActivity` to block on settlement
  observes the row instantly instead of racing the swap monitor's
  asynchronous first `SubscribeSwaps` update. The monitor still owns
  the live `SubscribeWallet` emit for that row — calling
  `projectAndEmit` here too would fan the same pending row out twice.
  The credit-backed pay path (`sendCreditInvoiceIntent`) instead calls
  `projectAndEmit` directly for its initial pending row: a credit-only
  pay has no swap session to emit a follow-up update at all, so the
  eager write must also be the live emit. A later monitor update for a
  mixed pay simply supersedes it.
- `WalletEntry.id` is the stable canonical id across the entire pending
  → terminal lifecycle for SEND-invoice and RECV (Lightning
  payment_hash), on-chain-send / cooperative-leave EXIT (the daemon's
  leave-job id / `send_job_id`), and DEPOSIT (`deposit-<address>`, keyed
  on the allocated boarding address surfaced on the confirmed history
  row). A unilateral EXIT still keys by the consumed VTXO outpoint. The
  pending → COMPLETE transition for EXIT/DEPOSIT lands via the
  derive/backfill pass; live cross-restart reconciliation is C2. See
  `doc.go`.
- Onchain SEND is routed through `RPCServer.SendOnChain` which delegates to
  `wallet.SendOnChainRequest`. Two modes: **sweep-all** (non-empty
  `SweepOutpoints` — drains those VTXOs exactly, no change, leave output
  absorbs fee under the #270 handshake) and **bounded** (selects VTXOs to
  cover `TargetAmountSat` + headroom, produces a change VTXO). The router
  calls `listLiveVTXOsForLeave` for sweep-all enumeration.
- `SendResponse.actual_amount_sat` carries the true outflow for sweep-all
  sends and SHOULD be echoed back before the send is treated as confirmed.
- **Onchain SEND is a one-shot**: after the intent is accepted the router
  immediately calls `JoinNextRound` so the queued leave intent is committed
  to the next round without a separate CLI step. If the implicit join fails,
  the error carries the explicit recovery hint (`ark rounds join`) so the
  leave intent — already queued in the round actor — is not stranded silently.
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
  mirror the ledger confirmation status and are keyed
  `deposit-<address>` from the confirmed row's
  `TransactionHistoryEntry.boarding_address`; every UTXO paid to that
  address is SUMMED into one row (`sumDepositsByAddress`) so a reused
  address shows its total. `Deposit` does NOT project a row — allocating
  an address is not a pending deposit — it only returns that id so a
  caller can correlate the eventual confirmed row. Per-address is the
  CONFIRMED phase only: unconfirmed boarding funds have no per-address
  source (the daemon exposes only aggregate `boarding_unconfirmed_sat`),
  so they surface via `Balance` and the single derive-path-only
  `boarding-unconfirmed` row, never projected into the store.
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
