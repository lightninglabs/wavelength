# swapwallet

## Purpose

Daemon-side implementation of the `walletdkrpc.WalletService` gRPC
subserver. It composes the swap subsystem (`swapclientserver`), the
cooperative-leave RPC, the daemon's wallet/admin surface, the boarding
ledger, the `credit` durable-actor subsystem, and the unilateral-exit
registry into one flat user-facing API: the seven wallet verbs (Create,
Unlock, Send, Recv, List, Balance, Exit) plus the supporting Deposit /
Status / SubscribeWallet methods. Sends/receives that are sub-dust or
otherwise credit-eligible are routed through the credit registry
transparently — the caller only sees wallet vocabulary.

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
  (narrow daemonrpc contract), `CreditRegistry` (lazy `actor.ActorRef` into
  the `credit` durable-actor subsystem; nil disables credit-backed routing),
  `ChainParams` (Bitcoin network — used to
  validate BOLT-11 invoice decoding in `PrepareSend` so a cross-network
  invoice is rejected before a send intent is issued), `ActivityStore`
  (canonical activity-log projector), plus wallet-level
  deadline, list-limit, and subscribe-buffer knobs.
- `RPCServer` interface — Narrow contract over `*darepod.RPCServer`
  covering every daemonrpc/darepod method swapwallet composes against:
  LeaveVTXOs, SendOnChain, SendOOR, ListVTXOs, ListTransactions, GetInfo,
  EstimateFee, GetBalance, NewAddress, NewWalletAddress, ListWalletUnspent,
  GenSeed, InitWallet, UnlockWallet, Unroll, GetUnrollStatus, ExitSummary,
  GetExitPlan, SweepWallet, JoinNextRound. The admin-shape methods
  (GenSeed/InitWallet/UnlockWallet/Unroll/GetUnrollStatus) are reachable
  BEFORE the swap runtime is live.
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
    backends consumed for LeaveVTXOs, SendOOR, ListVTXOs, ListTransactions,
    GetBalance, NewAddress)
  - `rpc/swapclientrpc` (swap-subsystem gRPC shape; ListSwaps,
    StartPay, StartReceive)
  - `swapclientserver` (typed `Backend` handle and runtime resume)
  - `credit` (`CreditMsg`/`CreditResp`, `StartCreditPayRequest`,
    `ListCreditOpsRequest` — durable credit-backed pay/recv routing and the
    credit projector poll)
  - `darepod` (`SwapBackend`, `ActivityStore` interfaces)
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
    `SendOnChainRequest`, `SendOORRequest`, `JoinNextRoundRequest`,
    `ListVTXOsRequest`, `ListTransactionsRequest`, `NewAddressRequest`,
    `GetBalanceRequest`, `GetInfoRequest`
  - → swapclientrpc (in-process via SwapService): `StartPayRequest`,
    `StartReceiveRequest`, `ListSwapsRequest`, `SubscribeSwapsRequest`
- **Receives**:
  - ← API: `walletdkrpc.{Create,Unlock,PrepareSend,Send,Recv,List,Balance,
    Deposit,Status,GetExitPlan,SweepWallet,Exit,ExitStatus,ExitSummary,
    SubscribeWallet,InspectActivity}Request`
- **Messages to/from**: Sends `credit.StartCreditPayRequest` /
  `credit.ListCreditOpsRequest` -> `credit` registry actor (via
  `Deps.CreditRegistry.Ask`); the credit projector loop polls
  `ListCreditOpsResponse` <- `credit` to fan terminal credit-op state onto
  `WalletEntry` rows.

## Invariants

- Admin handlers (`Create`/`Unlock`/`Exit`/`ExitStatus`) are
  admin-shape: they reach daemonrpc via the injected `RPCServer` and
  DO NOT depend on `Runtime`, router, recv, or history. Create and
  Unlock must work before the swap subsystem is live.
- Credit-backed routing is nil-safe: `Deps.CreditRegistry == nil` disables
  credit-only sends (falls back with `ErrSwapBackendUnavailable`) and the
  credit projector loop is a no-op, so builds without the credit subsystem
  wired pay nothing extra.
- A pay is **credit-only** (owned solely by the credit projector) when the
  server pins it to credit or `creditCoversSat` (overflow-safe) shows applied
  credit + planned top-up covers the full principal; otherwise it is
  **mixed** and the swap monitor loop stays the single terminal authority for
  the shared payment-hash row — the credit projector must never emit for a
  mixed pay.
- The credit projector polls on a coarse 5s tick
  (`creditProjectInterval`) and only re-emits an operation when its
  `credit.State` changed since the last poll, keyed by `OpID` in an
  in-process (non-durable) map that starts empty on restart.
- The periodic reconciler (`reconcileActivity`, `reconcileInterval`) runs
  two passes: a full-history re-derive/re-project over the low-volume
  DEPOSIT/EXIT kinds (`reconcilerKinds`), and a bounded recent-window pass
  (`rawOORReconcileWindow` = 100 rows) over the high-volume SEND/RECV
  kinds (`rawOORReconcileKinds`). The bounded pass exists because a raw
  out-of-round send/receive (`ark send oor` / `ark oor receive`) is
  neither swap-backed nor credit-backed, so it has no live projector and
  would otherwise land only at the next startup backfill (issue #903);
  paging the full SEND/RECV history every tick would be unbounded work,
  so only the most recent window is re-projected. Re-deriving swap/
  credit-backed SEND/RECV rows caught in that window is safe — change
  suppression makes an already-current row a no-op, so the reconciler
  never double-emits a row the monitor or credit poll already advanced.
- Background goroutines (monitor loop, deadline watcher, resume
  sweep) are anchored to the daemon root context, NEVER to RPC-call
  contexts. An RPC client disconnect cannot cancel in-flight work.
- `WalletEntry.id` is the stable canonical id across the entire pending
  → terminal lifecycle for SEND-invoice and RECV (Lightning
  payment_hash), on-chain-send / cooperative-leave EXIT (the daemon's
  leave-job id / `send_job_id`), and DEPOSIT (`deposit-<address>`, keyed
  on the allocated boarding address surfaced on the confirmed history
  row). A unilateral EXIT still keys by the consumed VTXO outpoint. The
  pending → COMPLETE transition for EXIT/DEPOSIT lands via the
  derive/backfill pass; live cross-restart reconciliation is C2. See
  `doc.go`.
- A completed cooperative-leave EXIT row is stamped with the settling
  round's on-chain coordinates (`progress.txid` /
  `progress.confirmation_height`) when the daemon's forfeited-VTXO lookup
  reports them (`collectForfeitedVTXOSettlements`). An old daemon that
  does not populate the VTXO settlement fields still marks the row
  COMPLETE, just without the txid/height.
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
  `pending_out_sat` sums `boarding_pending_sweep_sat +
  vtxo_pending_sat + vtxo_unilateral_exit_sat`. The two VTXO buckets
  carry value locked in an in-flight round / OOR spend and in a
  unilateral on-chain exit; folding them into `pending_out_sat` keeps
  the balance from momentarily reading zero mid-refresh, mid-spend, or
  mid-leave. Confirmed-but-not-yet-boarded UTXOs must NOT inflate
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
