# swapwallet

## Purpose

Daemon-side implementation of the `walletdkrpc.WalletService` gRPC
subserver. It composes the swap subsystem (`swapclientserver`), the
cooperative-leave RPC, the daemon's wallet/admin surface, the boarding
ledger, the unilateral-exit registry, and (when published) the durable
`credit` registry into one flat user-facing API: the seven wallet verbs
(Create, Unlock, Send, Recv, List, Balance, Exit) plus the supporting
Deposit / Status / SubscribeWallet / GetExitPlan / SweepWallet methods.
Send and Recv transparently route sub-dust or credit-eligible amounts
through the credit subsystem instead of a Lightning swap when the quote
calls for it; the caller never has to choose the rail.

The whole package lives behind `//go:build walletdkrpc && swapruntime` so
default builds avoid the swap executor's dependency graph.

## Key Types

- `Service` — gRPC handler implementing `walletdkrpc.WalletServiceServer`.
  Thin facade: each method dispatches to `router`, `receiver`,
  `history`, or admin proxy helpers; no business logic lives here.
  `Service.earmarkedCreditSat` (a `credit.EarmarkFunc`) reports the
  credit balance reserved by live prepared credit-backed sends so the
  daemon's auto-redeem sweep never redeems credits a prepared-but-unsent
  send is about to spend.
- `Runtime` — Owns the in-process swap lifecycle: synchronous
  resume-on-startup, deadline watcher (overlays stuck entries as
  FAILED with a `failureCode`), monitor loop (fans normalized updates to
  subscribers), and `startCreditProjectorLoop` (see below). Anchored to
  the daemon root context so an RPC client disconnect can never cancel
  in-flight work.
- `Deps` — Composition struct: `SwapBackend` (in-Go swap runtime),
  `SwapService` (gRPC-shaped swap subserver handle), `RPCServer`
  (narrow daemonrpc contract), `CreditRegistry`
  (`actor.ActorRef[credit.CreditMsg, credit.CreditResp]`, nil when the
  swap runtime did not publish it), `ChainParams` (Bitcoin network —
  used to validate BOLT-11 invoice decoding in `PrepareSend` so a
  cross-network invoice is rejected before a send intent is issued),
  plus wallet-level deadline, list-limit, and subscribe-buffer knobs.
- `RPCServer` interface — Narrow contract over `*darepod.RPCServer`
  covering every daemonrpc method swapwallet composes against:
  LeaveVTXOs, SendOnChain, SendOOR, ListVTXOs, ListTransactions,
  NewAddress, NewWalletAddress, ListWalletUnspent, GetInfo, EstimateFee,
  GetBalance, GenSeed, InitWallet, UnlockWallet, Unroll,
  GetUnrollStatus, GetExitPlan, SweepWallet, JoinNextRound. The
  admin-shape methods (GenSeed/InitWallet/UnlockWallet/Unroll/
  GetUnrollStatus) are reachable BEFORE the swap runtime is live.
- `router` — Dispatches Send. Invoice sends that a `QuotePay` reply
  marks as credit-using (`intentUsesCredit`) are routed through
  `sendCreditInvoiceIntent` to `Deps.CreditRegistry` instead of
  `SwapService.StartPay`; onchain previews select VTXOs via the shared
  `coinselect.LargestFirst` and price the leave with `estimateOnchainFee`
  (see `onchain_fee.go`).
- `receiver` — Drives Recv. A sub-dust invoice amount (below the
  operator's `GetDustLimit`) that available credit cannot top up to
  a viable vHTLC size is routed to `recvCredit`, which asks
  `Deps.CreditRegistry` to create a durable credit-backed receive
  instead of a swap-in session.
- `credit_projector.go` (`startCreditProjectorLoop` /
  `creditProjectorLoop`) — Background goroutine, anchored to
  `r.rootCtx` like the monitor loop, that polls
  `Deps.CreditRegistry.Ask(&credit.ListCreditOpsRequest{})` on a 5s
  ticker and projects state changes onto `WalletEntry` rows via
  `r.emit`. It is the only path that terminalizes credit-only pays and
  credit receives (no swap session backs them); a no-op when
  `CreditRegistry` is nil. Mixed pays are skipped — their shared
  payment-hash row stays owned by the swap monitor loop.
- `errors_grpc.go` (`ErrorMappingInterceptor` / `mapSentinel` /
  `statusSwapBackendUnavailable`) — Unary server interceptor and helpers
  that translate swapwallet sentinel errors into gRPC `status` errors
  carrying a `google.rpc.ErrorInfo` reason (from the
  `sentinelMappings` table), so SDK clients can branch on a stable
  machine-readable reason instead of string-matching. Errors that
  already carry a gRPC status pass through unchanged.
- `limits.go` (`checkReceiveLimits`) — Enforces the operator's
  advertised per-VTXO maximum (`ServerInfo.MaxVtxoAmount`) and total
  wallet balance cap (`ServerInfo.MaxUserBalance`) before a Recv opens a
  swap-in session. Both checks fail OPEN on a GetInfo/GetBalance error
  or when the terms are zero (disabled); they are advisory only — the
  operator re-validates VTXO creation server-side.
- `onchain_fee.go` (`estimateOnchainFee` / `fetchOnchainTerms`) —
  Prepare-time fee preview for a cooperative leave. Prefers the
  operator's dynamic `RPCServer.EstimateFee` quote
  (`SEND_QUOTE_STATUS_COMPLETE`); falls back to a local batch-size-1
  floor (`localOnchainFeeFloor`, `SEND_QUOTE_STATUS_LOCAL_ONLY`) derived
  from cached `GetInfo` terms when the operator is unreachable.
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
    GetBalance, NewAddress, EstimateFee, GetExitPlan, SweepWallet)
  - `rpc/swapclientrpc` (swap-subsystem gRPC shape; ListSwaps,
    StartPay, StartReceive, QuotePay, ListCredits)
  - `swapclientserver` (typed `Backend` handle and runtime resume)
  - `darepod` (`SwapBackend` interface, `ExitPlanRequest`/
    `SweepWalletRequest`)
  - `credit` (`CreditMsg`/`CreditResp` actor protocol, `CreditOpSummary`,
    `State` — durable credit-backed pay/receive routing and the credit
    projector's terminal-state polling)
  - `baselib/actor` (`ActorRef` — the typed handle `Deps.CreditRegistry`
    is Ask'd against)
  - `coinselect` (`LargestFirst` — shared VTXO selection for onchain-send
    previews, mirrors the daemon's own selection so a preview does not
    under-select relative to the real send)
  - `ledger` (account name constants for OOR ledger projection)
  - `btclog/v2` (subsystem logger)
  - `google.golang.org/grpc/status` + `errdetails` (`ErrorMappingInterceptor`
    maps sentinels to `google.rpc.ErrorInfo`-bearing statuses)
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
    `GetInfoRequest`, `EstimateFeeRequest`
  - → darepod (in-process via RPCServer): `ExitPlanRequest`,
    `SweepWalletRequest`
  - → swapclientrpc (in-process via SwapService): `StartPayRequest`,
    `StartReceiveRequest`, `ListSwapsRequest`, `SubscribeSwapsRequest`,
    `QuotePayRequest`, `ListCreditsRequest`
  - → credit (in-process via `Deps.CreditRegistry.Ask`):
    `StartCreditPayRequest`, `StartCreditReceiveRequest`,
    `ListCreditOpsRequest`
- **Receives**:
  - ← API: `walletdkrpc.{Create,Unlock,Send,Recv,List,Balance,Deposit,
    Status,Exit,ExitStatus,GetExitPlan,SweepWallet,
    SubscribeWallet}Request`

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
- Onchain SEND is routed through `router.sendOnchainIntent` →
  `RPCServer.SendOnChain` (`daemonrpc.SendOnChainRequest`, a oneof of
  `AmountSat` / `SweepAll`). **Exact-amount semantics**: a bounded send
  (`amount_sat`) lands exactly that many sats at the destination and the
  residual returns as a change VTXO under the #270 seal-time handshake;
  the earlier whole-VTXO-sweep overpay (issue #634) is gone. `sweep_all`
  drains every live VTXO with no change output. `SendOnChainRequest`
  registers the round intent atomically inside the daemon handler — the
  router does NOT call `JoinNextRound` itself for onchain SEND (that was
  an artifact of the previous `LeaveVTXOs`-based implementation).
  `PrepareSend`'s onchain *preview* is a separate, best-effort local
  calculation: it selects live VTXOs via `coinselect.LargestFirst`
  (target = `amt_sat` + operator-fee + dust headroom for a bounded send)
  and prices the leave with `estimateOnchainFee`, but the real send always
  re-selects and re-prices inside `SendOnChain`.
- `SendResponse.actual_amount_sat` carries the true outflow for sweep-all
  sends and SHOULD be echoed back before the send is treated as confirmed.
- `ListView` defaults to Activity. Only Activity honors
  `pending_only` and `kinds`; those filters are ignored for VTXOs
  and Onchain.
- VTXOs view filters out terminal internal states (FORFEITED, SPENT,
  FAILED) so the wallet view stays focused on actionable VTXOs.
- The runtime's deadline overlay elevates stuck PENDING entries to
  FAILED with `failure_reason="timed_out"` and
  `failure_code=ENTRY_FAILURE_CODE_TIMED_OUT` BEFORE filtering, so a
  stuck row appears as FAILED even when the caller asks for
  `pending_only=false`.
- **Credit-backed routing decisions are made once, at prepare/send time,
  and never re-derived from raw amounts later.** `router.prepareInvoice`
  calls `SwapService.QuotePay` and stashes the resulting
  `CreditPreview` on the `preparedSendIntent`; `intentUsesCredit` and
  `sendCreditInvoiceIntent`'s credit-only classification
  (`creditCoversSat`, overflow-safe via `saturatingAddSat`) both read
  that stashed preview. A credit-only pay (`MustUseCredit`, or applied +
  planned top-up already covers the principal) is handed to
  `Deps.CreditRegistry` under an idempotency key derived from the
  payment hash (`"pay:" + hex(paymentHash)`) and is the ONLY path that
  terminalizes that row (via `credit_projector.go`); a mixed pay is
  still handed to the credit registry (for the top-up) but its
  payment-hash row stays owned by the swap monitor loop, which is the
  single terminal authority for the Lightning leg — the projector
  explicitly skips non-`CreditOnly` pays to avoid a race between the two
  terminal sources.
- **`preparedSendStore.earmarkedCreditSat`** sums `maxCreditSat` across
  live (non-expired) prepared intents and is wired into the daemon's
  credit auto-redeem interlock via `cfg.Swap.CreditEarmarkSetter` in
  `register.go`. This MUST stay wired: without it, the auto-redeem sweep
  can redeem credits a prepared-but-unsent send has already earmarked,
  causing that send to fail credit-coverage checks it should have
  passed. An intent earmarks only for its TTL.
- **A sub-dust Recv falls back to a credit-backed receive** when the
  requested amount is below the operator's dust limit and available
  credit cannot top it up to a viable vHTLC size (`receiver.Recv`'s
  `plannedVHTLCSat` / `availableCreditSat` check); otherwise the receive
  still opens a normal swap-in vHTLC sized at `amt + availableCreditSat`.
  Receive-limit checks (`checkReceiveLimits`) run against whichever
  amount will actually be asked of the operator.
- **gRPC status mapping is centralized in `errors_grpc.go`.** Handlers
  return bare sentinel errors (`ErrInvalidDestination`,
  `ErrAmountExceedsVTXOLimit`, etc.) and `ErrorMappingInterceptor`
  translates them into a `status.Error` carrying a
  `google.rpc.ErrorInfo{Reason, Domain: walletdkrpc.FailureDomain}` via
  the `sentinelMappings` table. A handler that must return a pre-formed
  status directly (the readiness gate, admin proxies — anything running
  before the interceptor's return value is asserted on some path) MUST
  call `statusSwapBackendUnavailable()` rather than hand-rolling
  `status.Error(codes.Unavailable, ...)`, so the SDK can still
  reconstruct the typed sentinel. `ErrWalletNotReady` is deliberately
  absent from `sentinelMappings`: the wallet-readiness gate returns
  daemonrpc's own structured `WALLET_NOT_READY` status directly, so the
  bare sentinel never reaches the interceptor.
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
  When `Deps.SwapService` is set, `fetchBalance` additionally calls
  `ListCredits` and fills `CreditAvailableSat`/`CreditReservedSat`; a
  failed or absent credit lookup leaves those fields zero rather than
  failing the whole Balance call.

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
