# swapwallet

## Purpose

Daemon-side implementation of the `wavewalletrpc.WalletService` gRPC
subserver. It composes the swap subsystem (`swapclientserver`), the
cooperative-leave RPC, the daemon's wallet/admin surface, the boarding
ledger, the `credit` durable-actor subsystem, and the unilateral-exit
registry into one flat user-facing API: the seven wallet verbs (Create,
Unlock, Send, Recv, List, Balance, Exit) plus the supporting Deposit /
Status / SubscribeWallet methods. Sends/receives that are sub-dust or
otherwise credit-eligible are routed through the credit registry
transparently — the caller only sees wallet vocabulary.

The whole package lives behind `//go:build wavewalletrpc && swapruntime` so
default builds avoid the swap executor's dependency graph.

## Key Types

- `Service` — gRPC handler implementing `wavewalletrpc.WalletServiceServer`.
  Thin facade: each method dispatches to `router`, `receiver`,
  `history`, or admin proxy helpers; no business logic lives here.
- `Runtime` — Owns the in-process swap lifecycle: synchronous
  resume-on-startup, deadline watcher (overlays stuck entries as
  FAILED), monitor loop (fans normalized updates to subscribers).
  Anchored to the daemon root context so an RPC client disconnect can
  never cancel in-flight work.
- `Deps` — Composition struct: `SwapBackend` (in-Go swap runtime),
  `SwapService` (gRPC-shaped swap subserver handle), `RPCServer`
  (narrow waverpc contract), `CreditRegistry` (lazy `actor.ActorRef` into
  the `credit` durable-actor subsystem; nil disables credit-backed routing),
  `ChainParams` (Bitcoin network — used to
  validate BOLT-11 invoice decoding in `PrepareSend` so a cross-network
  invoice is rejected before a send intent is issued), `ActivityStore`
  (canonical activity-log projector), plus wallet-level
  deadline, list-limit, and subscribe-buffer knobs.
- `RPCServer` interface — Narrow contract over `*waved.RPCServer`
  covering every waverpc/waved method swapwallet composes against:
  LeaveVTXOs, SendOnChain, SendOOR, ListVTXOs, ListTransactions, GetInfo,
  EstimateFee, GetBalance, NewAddress, NewWalletAddress, ListWalletUnspent,
  GenSeed, InitWallet, UnlockWallet, Unroll, GetUnrollStatus, ExitSummary,
  GetExitPlan, SweepWallet, JoinNextRound. The admin-shape methods
  (GenSeed/InitWallet/UnlockWallet/Unroll/GetUnrollStatus) are reachable
  BEFORE the swap runtime is live.
- `WalletEntry` (re-exported from wavewalletrpc) — Flat row type the entire
  history/streaming surface returns. Every internal correlator
  (session_id, round_id, settlement_type, mailbox subtype) is dropped
  before responding.
- `ListView` (re-exported from wavewalletrpc) — Selects between Activity
  (merged WalletEntry stream), VTXOs (live inventory), and Onchain
  (boarding + sweep) views. Default is Activity.

## Relationships

- **Depends on**:
  - `rpc/wavewalletrpc` (generated gRPC stubs and request/response shapes)
  - `waverpc` (admin RPCs proxied by Create/Unlock/Exit and the
    backends consumed for LeaveVTXOs, SendOOR, ListVTXOs, ListTransactions,
    GetBalance, NewAddress)
  - `rpc/swapclientrpc` (swap-subsystem gRPC shape; ListSwaps,
    StartPay, StartReceive)
  - `swapclientserver` (typed `Backend` handle and runtime resume)
  - `credit` (`CreditMsg`/`CreditResp`, `StartCreditPayRequest`,
    `ListCreditOpsRequest` — durable credit-backed pay/recv routing and the
    credit projector poll)
  - `waved` (`SwapBackend`, `ActivityStore` interfaces)
  - `ledger` (account name constants for OOR ledger projection)
  - `btclog/v2` (subsystem logger)
- **Depended on by**:
  - `cmd/waved` (`wavewalletrpc.go` registers the subserver behind the
    wavewalletrpc build tag)
  - `sdk/wavewalletdk` (gomobile-friendly SDK wraps the same gRPC service)
- **Sends**:
  - → waverpc (in-process via RPCServer):
    `InitWalletRequest`, `UnlockWalletRequest`, `GenSeedRequest`,
    `UnrollRequest`, `GetUnrollStatusRequest`, `LeaveVTXOsRequest`,
    `SendOnChainRequest`, `SendOORRequest`, `JoinNextRoundRequest`,
    `ListVTXOsRequest`, `ListTransactionsRequest`, `NewAddressRequest`,
    `GetBalanceRequest`, `GetInfoRequest`
  - → swapclientrpc (in-process via SwapService): `StartPayRequest`,
    `StartReceiveRequest`, `ListSwapsRequest`, `SubscribeSwapsRequest`
- **Receives**:
  - ← API: `wavewalletrpc.{Create,Unlock,PrepareSend,Send,Recv,List,Balance,
    Deposit,Status,GetExitPlan,SweepWallet,Exit,ExitStatus,ExitSummary,
    SubscribeWallet,InspectActivity}Request`
- **Messages to/from**: Sends `credit.StartCreditPayRequest` /
  `credit.ListCreditOpsRequest` -> `credit` registry actor (via
  `Deps.CreditRegistry.Ask`); the credit projector loop polls
  `ListCreditOpsResponse` <- `credit` to fan terminal credit-op state onto
  `WalletEntry` rows.

## Invariants

- Admin handlers (`Create`/`Unlock`/`Exit`/`ExitStatus`) are
  admin-shape: they reach waverpc via the injected `RPCServer` and
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
- Onchain SEND is routed through `RPCServer.SendOnChain` which delegates to
  `wallet.SendOnChainRequest`. Two modes: **sweep-all** (non-empty
  `SweepOutpoints` — drains those VTXOs exactly, no change, leave output
  absorbs fee under the #270 handshake) and **bounded** (selects VTXOs to
  cover `TargetAmountSat` + headroom, produces a change VTXO). The router
  calls `listLiveVTXOsForLeave` for sweep-all enumeration.
- `SendResponse.actual_amount_sat` carries the true outflow for sweep-all
  sends and SHOULD be echoed back before the send is treated as confirmed.
- **Cooperative-leave EXIT fee**: at completion
  (`applyCooperativeLeaveForfeited`), the forfeited source VTXO's
  settlement carries the forfeit round's operator fee (from the daemon
  ledger via the `ListVTXOsByStatus` fee join), which is stamped onto
  `WalletEntry.fee_sat`. A sweep-all row (marked via
  `OnchainAddressRequest.sweep_all`, set by `leaveEntryStub`) also nets
  the fee back out of its gross pending amount, so every completed EXIT
  reads amount = destination-received, fee = cost on top.
- **Unilateral EXIT fee**: `applyUnrollStatus` applies the same shape on
  a COMPLETED unroll: `GetUnrollStatusResponse.exit_cost_sat` (the
  ledger's confirmed onchain_fee_paid exit leg) becomes `fee_sat` and is
  netted out of the row's gross VTXO amount. Zero cost (old daemon, or
  an exit predating exit-cost accounting) leaves the row untouched.
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
  caller can correlate the eventual confirmed row. Before confirmation,
  the in-process daemon address registry is correlated with zero-conf wallet
  UTXOs so the live overlay normally uses the same `deposit-<address>` id.
  Older embeddings and ambiguous multi-address balances retain the ephemeral
  aggregate `boarding-unconfirmed` fallback. Neither live overlay is projected
  into the store or resumable event log.
- **`Balance` projection** maps waverpc fields onto the wavewalletrpc
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

- [docs/wavewalletrpc_build.md](../docs/wavewalletrpc_build.md) — Build modes,
  make targets, what the wavewalletrpc tag enables.
- [docs/wavewalletdk_integration.md](../docs/wavewalletdk_integration.md) —
  How `sdk/wavewalletdk` wraps the same gRPC service for embedded hosts.
- [docs/swap_background_execution.md](../docs/swap_background_execution.md)
  — Daemon-side swap lifecycle the runtime composes over.
- [rpc/wavewalletrpc/CLAUDE.md](../rpc/wavewalletrpc/CLAUDE.md) — Proto schema
  and per-message invariants.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
