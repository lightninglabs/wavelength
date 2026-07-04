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

Two subsystems layer on top of that core RPC surface. A canonical
activity-log projector (`projector.go`, `credit_projector.go`) durably
persists every emitted `WalletEntry` into the daemon's `ActivityStore`
(`activity_entries` current-state rows plus an append-only
`activity_events` log), so `List(ACTIVITY)` and `SubscribeWallet`'s
snapshot read a keyset-paginated store instead of re-deriving on every
call. A credit-routing layer (`router.go`, `recv.go`, `limits.go`) sends
sub-dust receives and credit-quoted pays through the `credit` package's
durable registry actor instead of a pure Lightning swap. `admin.go` also
gained two backing-wallet admin RPCs, `GetExitPlan` and `SweepWallet`,
that preview unilateral-exit funding and normal-wallet sweeps.

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
  (narrow daemonrpc contract), `CreditRegistry`
  (`actor.ActorRef[credit.CreditMsg, credit.CreditResp]` — durable
  credit-registry actor handle used to route credit-backed Send/Recv; nil
  when the swap runtime did not publish it, in which case the router
  declines credit-backed sends and sub-dust receives fail), `ActivityStore`
  (`darepod.ActivityStore` — the canonical activity-log projector the
  runtime writes each emitted `WalletEntry` through; nil disables
  projection), `ChainParams` (Bitcoin network — used to
  validate BOLT-11 invoice decoding in `PrepareSend` so a cross-network
  invoice is rejected before a send intent is issued), plus wallet-level
  deadline, list-limit, and subscribe-buffer knobs.
- `RPCServer` interface — Narrow contract over `*darepod.RPCServer`
  covering every daemonrpc method swapwallet composes against:
  LeaveVTXOs, SendOnChain, SendOOR, ListVTXOs, ListTransactions, NewAddress,
  EstimateFee, GetInfo, GetBalance, GenSeed, InitWallet, UnlockWallet,
  Unroll, GetUnrollStatus, GetExitPlan, SweepWallet, JoinNextRound. The
  admin-shape methods (GenSeed/InitWallet/UnlockWallet/Unroll/
  GetUnrollStatus/GetExitPlan/SweepWallet) are reachable BEFORE the swap
  runtime is live.
- `WalletEntry` (re-exported from walletdkrpc) — Flat row type the entire
  history/streaming surface returns. Every internal correlator
  (session_id, round_id, settlement_type, mailbox subtype) is dropped
  before responding.
- `ListView` (re-exported from walletdkrpc) — Selects between Activity
  (merged WalletEntry stream), VTXOs (live inventory), and Onchain
  (boarding + sweep) views. Default is Activity.
- `Runtime.project` / `projectAndEmit` / `backfillActivity` (projector.go) —
  Best-effort write path into the canonical activity log: upserts the
  current-state row and appends the transition event via
  `Deps.ActivityStore`, then fans the entry out to live subscribers. A
  projection failure never suppresses the emit. `backfillActivity` seeds
  the store once at startup from the legacy derive-on-read collectors
  (`history.deriveActivity`), which is otherwise retained only for
  store-less/test builds.
- `onchainFeeQuote` / `onchainTerms` (onchain_fee.go) — Prepare-time fee
  preview for a cooperative leave. Prefers the operator's `EstimateFee`
  quote (COMPLETE); falls back to a local batch-size-1 floor
  (`localOnchainFeeFloor`, LOCAL_ONLY) derived from cached `GetInfo` terms
  when the operator is unreachable.
- `mapSentinel` / `ErrorMappingInterceptor` (errors_grpc.go) — Unary gRPC
  interceptor that maps a swapwallet sentinel error to a `status.Status`
  carrying a machine-readable `google.rpc.ErrorInfo` reason (the
  `walletdkrpc` wire contract), so SDK clients can branch on failure cause
  without string matching. Errors already carrying a gRPC status pass
  through unchanged.

## Relationships

- **Depends on**:
  - `rpc/walletdkrpc` (generated gRPC stubs and request/response shapes)
  - `daemonrpc` (admin RPCs proxied by Create/Unlock/Exit and the
    backends consumed for LeaveVTXOs, ListVTXOs, ListTransactions,
    GetBalance, NewAddress)
  - `rpc/swapclientrpc` (swap-subsystem gRPC shape; ListSwaps,
    StartPay, StartReceive)
  - `swapclientserver` (typed `Backend` handle and runtime resume)
  - `darepod` (`SwapBackend`, `ActivityStore` interfaces; `ExitPlanRequest`/
    `SweepWalletRequest` for the backing-wallet admin RPCs)
  - `credit` (durable credit-registry actor; routes credit-backed
    Send/Recv via `StartCreditPayRequest`, `StartCreditReceiveRequest`,
    `ListCreditOpsRequest`)
  - `coinselect` (shared largest-first VTXO selection used to size the
    onchain-send preview the same way the daemon selects for the real
    send)
  - `db` / `db/sqlc` (`ActivityProjection` DTO and `ActivityEntry` row
    type for the canonical activity-log projection)
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
    `SendOnChainRequest`, `SendOORRequest`, `EstimateFeeRequest`,
    `JoinNextRoundRequest`, `ListVTXOsRequest`,
    `ListTransactionsRequest`, `NewAddressRequest`, `GetBalanceRequest`,
    `GetInfoRequest`
  - → darepod (in-process via RPCServer): `ExitPlanRequest`,
    `SweepWalletRequest`
  - → swapclientrpc (in-process via SwapService): `StartPayRequest`,
    `StartReceiveRequest`, `ListSwapsRequest`, `SubscribeSwapsRequest`,
    `QuotePayRequest`, `ListCreditsRequest`
  - → credit (in-process via `Deps.CreditRegistry` actor):
    `StartCreditPayRequest`, `StartCreditReceiveRequest`,
    `ListCreditOpsRequest`
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
  pending and confirmed in v1; see `doc.go`. A cooperative-leave EXIT row
  CAN still flip pending → COMPLETE in-process (once the source VTXO is
  terminally forfeited) without a restart; the id-sharing gap only bites
  across a restart, when the daemon cannot recreate the original
  counterparty/note from durable state alone.
- Onchain SEND is routed through `RPCServer.SendOnChain` which delegates to
  `wallet.SendOnChainRequest`. Two modes: **sweep-all** (non-empty
  `SweepOutpoints` — drains those VTXOs exactly, no change, leave output
  absorbs fee under the #270 handshake) and **bounded** (selects VTXOs
  through the shared `coinselect.LargestFirst` to cover `TargetAmountSat` +
  operator-fee + dust headroom, produces a change VTXO). The router calls
  `listLiveVTXOsForLeave` for VTXO enumeration and `estimateOnchainFee` for
  the fee preview.
- A **bounded** onchain send lands exactly `amt_sat` at the destination and
  returns the remainder as change, so it does not overpay; only a
  **sweep_all** send drains WHOLE selected VTXOs and can exceed the
  requested figure. `SendResponse.actual_amount_sat` carries the true
  outflow and SHOULD be echoed back before the send is treated as
  confirmed.
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
  `credit_available_sat` / `credit_reserved_sat` are best-effort: a
  `ListCredits` failure or a nil `SwapService` leaves them zero rather
  than failing the whole `Balance` call.
- **Canonical activity log**: the runtime dual-writes every emitted
  `WalletEntry` into `Deps.ActivityStore` (`activity_entries` +
  `activity_events`) via `projectAndEmit`. `List(ACTIVITY)` and
  `SubscribeWallet`'s `include_existing` snapshot read the store through
  an opaque, immutable `(created_at_unix, canonical_id)` keyset cursor —
  the feed is newest-by-creation, not newest-by-update. When no store is
  wired, both fall back to `history.deriveActivity` (the pre-store
  derive-on-read merge). Producers with no ongoing projector (confirmed
  boarding DEPOSIT, daemon-side sweep/EXIT rows from `ListTransactions`)
  only reach the store via the startup `backfillActivity` pass, so they
  surface in `List` after the next restart rather than immediately; see
  `doc.go` V1 LIMITATIONS.
- **Credit-backed Send/Recv routing**: a Recv below the operator's dust
  limit, or a Send whose `QuotePay`/`prepareInvoice` quote reserves or
  requires credits (`intentUsesCredit`), routes through
  `Deps.CreditRegistry` instead of the direct swap path
  (`sendCreditInvoiceIntent`, `recvCredit`). A mixed pay (partial credit +
  Lightning) still shares its payment-hash row with a swap session owned
  by the swap monitor, which stays the single terminal authority for that
  row; `creditProjectorLoop` polls the registry every
  `creditProjectInterval` (5s) and projects only credit-only pays and
  credit receives. `preparedSendStore.earmarkedCreditSat` reserves credit
  balance for live prepared-but-unsent credit sends so the daemon's
  auto-redeem sweep does not redeem credits a prepared send is about to
  spend.
- `checkReceiveLimits` (limits.go) enforces the operator's per-VTXO and
  total-balance caps on a Recv amount, but fails OPEN throughout: a
  missing/errored `GetInfo` or `GetBalance`, or a zero-valued limit,
  skips the check rather than blocking a legitimate receive. It is a
  pre-flight UX convenience, never a security boundary — the operator
  re-validates VTXO creation server-side.
- Swapwallet sentinel errors (`errors.go`) are mapped to gRPC status codes
  with a machine-readable `ErrorInfo` reason via
  `ErrorMappingInterceptor`/`mapSentinel` (`errors_grpc.go`). An error
  that already carries a gRPC status is passed through unchanged, so a
  handler that deliberately chose a code (e.g. the admin-shape readiness
  gate, via `statusSwapBackendUnavailable`) is never second-guessed.

## Deep Docs

- [docs/walletdkrpc_build.md](../docs/walletdkrpc_build.md) — Build modes,
  make targets, what the walletdkrpc tag enables.
- [docs/walletdk_integration.md](../docs/walletdk_integration.md) —
  How `sdk/walletdk` wraps the same gRPC service for embedded hosts.
- [docs/swap_background_execution.md](../docs/swap_background_execution.md)
  — Daemon-side swap lifecycle the runtime composes over.
- [docs/canonical_activity_log_design.md](../docs/canonical_activity_log_design.md)
  — Design of the `activity_entries`/`activity_events` store this
  package's projector writes.
- [docs/credit_system.md](../docs/credit_system.md) and
  [docs/credit_durable_actor_design.md](../docs/credit_durable_actor_design.md)
  — The `credit` package's server-held balance and durable-actor design
  that `Deps.CreditRegistry` routes credit-backed Send/Recv through.
- [rpc/walletdkrpc/CLAUDE.md](../rpc/walletdkrpc/CLAUDE.md) — Proto schema
  and per-message invariants.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
