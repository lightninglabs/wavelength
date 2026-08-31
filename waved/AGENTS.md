# waved

## Purpose

Top-level daemon orchestrator that wires the wallet backend, mailbox
transport, chain backend, database, and all domain actors into a running
system with a gRPC API.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/wavelength/waved.<Symbol>`.

- `Server` — main daemon. Owns the wallet, DB, chainsource actor, gRPC
  server, and `ActorSystem`. Caches `localMailboxID` (pubkey-derived),
  `authSigHex` (Schnorr auth), `clientKeyDesc` (the stable daemon identity
  descriptor, behind `clientKeyDescMu`), `mailboxAuthSigs` (per-recipient
  mailbox auth signature memo, behind `mailboxAuthSigsMu`), and a single
  `clk` (`clock.Clock`) shared by all sub-stores for deterministic time
  injection.
- `RPCServer` — implements the gRPC `DaemonService`. Most write RPCs
  (`Board`, `SendVTXO`, `SendOOR`, `SweepBoardingUTXOs`, `SendOnChain`)
  validate input locally then `Ask` the relevant actor; `GetRound` and
  `ListVTXOs` merge live actor state with persisted SQL rows, while
  `GetFeeHistory` and `ListTransactions` are pure SQL reads
  (`rpc_fees.go`).
- `Config` — daemon configuration: wallet backend selection, mailbox/chain
  backend wiring, `OORConfig`/`OORLimitsConfig` (receive safety caps),
  `UnrollConfig` (unilateral-exit fee-bump cadence and cap), and
  `MaxOperatorFeeSat` (the #270 seal-time fee-cap validated in
  `Config.Validate()`).
- `WalletState` — `None` / `Locked` / `Ready` wallet lifecycle.
- `WalletRecoveryResult` — counters returned by the in-process,
  post-unlock recovery hook.
- `UnrollConfig` / `OORConfig` — subsystem tunables; see `Config.Validate()`
  for the invariants each enforces.

## Relationships

- **Depends on**: `baselib/actor`, `btcwbackend`, `chainbackends`,
  `chainsource`, `lib/actormsg`, `db`, `ledger`, `round`, `txconfirm`,
  `unroll`, `vtxo`, `wallet`, `walletcore`, `oor`, `serverconn`, `indexer`,
  `arkrpc`, `lndbackend`, `fraud`, `gateway`, `rpc/restclient`,
  `vhtlcrecovery`, `vhtlcrecovery/coordinator`, `vhtlcrecovery/unrollpolicy`.
- **Depended on by**: `cmd/waved`.

## Invariants

- The lnd wallet account (`lnd.account`, empty = lnd's `default`) bounds what
  this daemon may **spend**: `ListWalletUnspent` (fee inputs and the exit
  preflight), `NewWalletAddress` (the deposit address), and the
  `lndUnrollWallet` fund/sign/change triple all resolve it through
  `Server.lndWalletAccount()`. Observation deliberately does not:
  `listBackingWalletUnspent` stays unfiltered because
  `fetchUnconfirmedBoardingBalance` and `ListUnconfirmedBoardingUTXOs` exist
  to see imported boarding scripts, which live in lnd's watch-only account
  and belong to no wallet account. Scoping that dispatcher empties both, and
  does so even with no account configured, since lnd reads an empty account
  as *every* account but `"default"` as a real filter.
- `validateLndAccount` refuses to start on a configured account that is
  missing, not taproot-scoped, or watch-only. Without it each of those fails
  later and worse — a missing account silently filters every UTXO away, a
  wrong-scoped one funds and signs but cannot derive a fresh script, and a
  watch-only one fails at signing after inputs are leased.
- `Server.run` registers a deferred `actorSystem.Shutdown()` **before** the
  deferred `db.Close()` so in-flight actor DB transactions drain before the
  connection pool tears down.
- Wallet transitions `None → Locked → Ready` (or direct to `Ready` if a seed
  is provided). Three wallet backends: LND, lightweight (`lwwallet`), or
  neutrino-backed (`btcwallet` via `btcwbackend`).
- `WaitForWalletServicesReady` resolves after wallet-dependent actors and
  mailbox ingress start, or with the first error before that boundary.
  `DaemonReady` remains the later full-startup signal.
- `Server.RecoverWalletState` is an in-process, post-unlock retry seam. It
  keeps the supplied context through the scan; idempotent writes make a partial
  scan safe to retry.
- Mailbox IDs are derived from identity pubkeys via
  `serverconn.PubKeyMailboxID`, not config strings. The operator's remote
  mailbox ID and pubkey are fetched via direct gRPC (`fetchCurrentOperatorPubKey`)
  before the mailbox runtime starts.
- **Every outbound mailbox edge is wrapped in
  `serverconn.NewAuthenticatedMailboxClient`**, so `Send`, `Pull`, and
  `AckUpTo` all carry `x-mailbox-auth-sig`. The operator authorizes a mailbox
  RPC either from the TLS client certificate bound to the caller's mailbox ID
  or from that header, and an operator terminating TLS at a proxy never sees a
  client certificate — signing unconditionally keeps the operator's TLS
  posture out of client config. Both `connectOperatorClients` arms (gRPC and
  REST) and `newMailboxEdge` wrap; the last is latent today but exists so a
  future caller cannot silently lose the header.
- **Mailbox auth signatures are memoized per recipient** in `mailboxAuthSig`.
  The digest is `TaggedHash("mailbox-auth", identityPubKey ||
  recipientMailboxID)` and does not vary with the request, so one signature
  serves the life of the key; signing per RPC would put a wallet round trip in
  front of every long-poll `Pull`. Two properties are load-bearing:
    - The wallet call happens with `mailboxAuthSigsMu` **released**.
      `sync.Mutex.Lock` is not context-aware, so holding it across the round
      trip would serialize the whole mailbox edge behind one signature and
      stall egress, ingress, heartbeat, and ack together on a wedged wallet.
      Two callers racing a cold recipient may both sign, which is harmless —
      the digest is deterministic.
    - The map **grows with distinct recipients and is never evicted**. The
      operator edge alone contributes two (`Send` addresses the compound
      `operator:client` mailbox; `Pull`/`AckUpTo` address the plain local
      one), and `RPCServer.SignMailboxAuth` adds one per-swap mailbox
      (`client:payment_hash`) per out-swap. This is growth, not a constant.
  The signing round trip is bounded by `mailboxAuthSignTimeout` (30 s) rather
  than inheriting the caller's context, because the ingress puller builds its
  context with no deadline at all.
- Public network endpoint defaults live in `defaultNetworkEndpoints`
  (`config.go`). `mainnet`, `testnet3`, and `signet` resolve to the Lightning
  Labs deployments; `regtest`/`simnet` keep the historical localhost
  endpoints. The mainnet **REST** hosts are declared but not yet routable —
  the external NLB and dual-SAN certificates cover only the gRPC names — so
  they stay dark pending the ingress work tracked in lightning-infra#3749.
  Changing a default here changes where an existing user's daemon dials on
  restart; treat it as a deployment change, not a constant tweak.
- All sub-stores share the single `s.clk` clock assigned in `NewServer`; new
  code must not call `clock.NewDefaultClock()` directly, use `s.clk`.
- Actor startup order in `startWalletDependentActors`: VTXO manager, then
  round actor, then the unroll subsystem (`initUnrollSubsystem`), then the
  OOR actor (`initOORActor`). The VTXO manager is constructed with a
  `vtxo.LazyChainResolver` placeholder that `initUnrollSubsystem` fills in
  later; anything needing that seam must run after `initUnrollSubsystem`.
- `initUnrollSubsystem` boot ordering is policy-preserving.
  `recoverySvc.RestoreNonTerminal` (in-flight vHTLC recovery jobs, each
  carrying its durable exit policy) runs **before** the chain resolver is
  `Set()`; the force-exit admissions it drives through the VTXO manager are
  buffered by the `LazyChainResolver` and replayed to the unroll registry the
  instant the resolver is wired. The registry is first-writer-wins on exit
  policy, so the generic orphan-job scan (`recoverOrphanedUnrollJobs`) runs
  **after** `Set()` and is itself policy-carrying: it is handed a per-outpoint
  exit-policy map (`recoveryExitPolicies`, built from the recovery store) and
  re-admits each orphaned recovery target under its own vHTLC exit policy
  rather than mislabeling it as a standard timeout.
- The chain-resolver→unroll bridge (`ensureUnrollFromExpiring`) maps a VTXO
  `ExpiringNotification`'s trigger and optional exit policy into the registry's
  `EnsureUnrollRequest`. `unrollStartTrigger` converts the string-typed
  `actormsg.UnrollTrigger` (kept string-typed to avoid a `vtxo → unroll` import
  cycle) into `unroll.StartTrigger`; an empty or unknown trigger admits as
  critical expiry. A `None` exit policy leaves the registry on its standard
  VTXO timeout policy.
- `ManagerConfig.CriticalExitAssessor` is wired to
  `RPCServer.assessAutomaticCriticalExit`, closing the loop in the other
  direction: before a critically-expiring VTXO escalates itself into
  unilateral exit, the VTXO actor asks waved whether the backing wallet can
  actually fund and relay the exit package. Both that callback and the manual
  `GetExitPlan` / `Unroll` path route through the single
  `assessUnrollFeasibility` helper (wallet snapshot → `resolveExitLineage` →
  `unroll.PlanExitFunding`), so automatic and hand-typed admission cannot
  disagree about executability. `automaticExitDecisionReason` renders the
  first failed `unroll.ExitFeasibility` invariant into the short string the
  VTXO actor logs. Because this is a plain function seam, `vtxo` gains no
  compile-time dependency on `unroll` or `waved`. An error from the helper is
  returned as-is and the VTXO actor deliberately falls back to the direct
  exit, so a wallet-query failure can never suppress the safety path.
- The fraud watcher (`initFraudWatcher`) is wired with `VTXOManagerRef`, so
  fraud spends drive exits through the VTXO manager — the same admission path
  as manual, critical-expiry, and vHTLC recovery exits — rather than talking to
  the unroll registry directly.
- The vHTLC recovery service is wired with an `Exiter: managerExitAdmitter`, a
  `ForceExit` seam that `Ask`s the VTXO manager to force a materialized
  recovery target into unilateral exit. The target materializer
  (`EnsureRecoveryTarget`) persists the descriptor directly into
  `VTXOStatusUnilateralExit` (not `VTXOStatusSpending`) so the exiting coin is
  excluded from the live/coin-selection query and cannot leak back into a
  cooperative round as a forfeit; the boot-time orphan scan re-admits it on
  restart.
- Boarding-sweep transaction construction, fee estimation, spend watching,
  and startup resumption live inside the **wallet actor**
  (`wallet.Ark.handleSweepBoardingUTXOs` / `handleResumeBoardingSweeps` in
  `wallet/boarding_sweep_actor.go` and `wallet/boarding_sweep.go`), not in
  waved. `RPCServer.SweepBoardingUTXOs` only validates the request and
  `Ask`s the wallet actor; waved supplies the boarding store
  (`newBoardingStore`) and the backend-specific sweep-wallet adapter
  (`newSweepWallet`, one of `lndUnrollWallet` / `lwUnrollWallet` /
  `btcwUnrollWallet`), which is structurally compatible with both
  `unroll.SweepWallet` and the wallet actor's `SweepSigner`.
- `LeaveVTXOs` filters its targets through `admitLeaveTargets` before
  dispatch, which runs `vtxo.CheckForfeitAdmission` over each descriptor.
  The two selection modes fail in opposite directions on purpose: an
  explicitly named outpoint is refused with `FailedPrecondition` naming
  the round that holds it, since silently dropping it would report a
  queued leave that never happens, while a `selection=all` sweep drops it
  (`ListLiveVTXOs` returns every non-terminal VTXO, so one in-flight coin
  would otherwise sink the batch). Every drop is logged with its outpoint
  and admission error and counted into `skipped_count`, so `queued_count=0`
  over a fully committed wallet stays distinguishable from an empty one.
  The filter is advisory; the VTXO FSM refuses a late claim in both
  `PendingForfeit` and `Forfeiting`.
- `GetExitPlan` reports a round commitment as an **advisory**, never as a
  per-entry error: the entry is still priced, and `CanStart` is lowered
  with `unroll.ExitRoundCommitted` plus a `RoundCommitment` naming the
  coin. This must stay an advisory because `Unroll` short-circuits only on
  `VTXOStatusUnilateralExit` and the FSM escalates a manual trigger from
  both committed states — so the exit being warned about is one the exit
  command performs, and it is the only recovery when the operator is
  unreachable and the commitment never confirms. Failing the entry would
  contradict `Unroll` and withhold the funding figures that recovery needs.
- `RefreshVTXOs` dry-run short-circuits before the wallet-ready gate
  (LeaveVTXOs parity) and attaches a best-effort advisory fee estimate
  (`rpc_refresh_estimate.go`): explicit outpoints are deduped and
  resolve via `vtxoStore.GetVTXO` (unknown or non-live outpoint =
  InvalidArgument, mirroring the --all LiveState filter), operator
  quotes go through the `EstimateFee` proxy deduped on (amount,
  remaining blocks) with remaining clamped to >= 1, and the
  free-refresh waiver is computed locally from the cached operator
  terms (the operator's EstimateFee does not apply it). Estimate
  failures set `estimate_error` and never fail the preview. The real
  refresh path still gates on wallet readiness.
- `SendVTXO` enforces `maxRecipients = 256`, rejects per-recipient amounts
  outside `(0, MaxSatoshi]`, and uses overflow-safe summation; the wallet
  actor repeats these checks as defense-in-depth.
- `SendOOR` with custom inputs serializes concurrent calls on the same
  outpoints via `reserveCustomInputs`; the release function is deferred on
  both success and failure.
- `SendOOR` maps `oor.ErrIdempotencyKeyConflict` to `codes.AlreadyExists`
  after releasing the freshly selected VTXO locks, so it never reports success
  under a caller key the deterministic session cannot retain. This includes a
  keyed retry over a live durable session that was originally admitted
  keylessly. A terminal failed outgoing row with no immutable attempt can be
  rebound only by rebuilding the same deterministic operation; its new key and
  proof still commit before transport enqueue.
- `resolveExistingOORRecipientOutpoints` (the keyed-replay path that rebuilds
  `SendOORResponse.RecipientOutpoints` without reselecting wallet inputs)
  reads the immutable dispatch attempt, not the mutable session snapshot. The
  sender can ingest its own OOR change under the same session id without
  hiding the outgoing identity. The proof covers caller recipients, not the
  separately added wallet change output. Exact recipient reordering returns
  the same distinct outpoints in caller order. A changed count, amount, or
  script is rejected with `codes.AlreadyExists`; malformed durable data is
  `codes.DataLoss`. A legacy binding with no canonical request returns the
  stable session id without recipient outpoints and never sends again. If the
  current outgoing lifecycle is known to have failed, replay returns status
  `failed` without claiming that its recipient outpoints exist. A same-key
  retry that reaches the in-memory admission winner before its attempt commits
  returns the stable session id with no unproven outpoints; a later retry
  resolves them from the committed attempt.
- `Unroll` / `GetUnrollStatus` return `codes.Unavailable` (not `Internal`)
  when the unroll subsystem refs are not yet set, so clients can retry.
- `Unroll` must set `ForceUnrollRequest.Trigger` explicitly to
  `actormsg.UnrollTriggerManual`. The zero value admits as
  `UnrollTriggerCriticalExpiry`, so omitting it records a hand-typed unroll as
  the expiry safety net and the job's persisted provenance names a trigger
  that never fired. The distinction is not cosmetic: `ForfeitingState`
  suppresses a critical-expiry exit once the forfeit signature has issued but
  still honours a manual one.
- `NewReceiveScript` treats a non-empty idempotency key as one durable
  allocation. The owned-script store records the key locator, script, operator
  terms, absolute expiry, stable mailbox RPC key, and completion evidence
  before indexer registration. Pending retry reuses those artifacts; completed
  replay returns without the indexer while its window is active. Expired replay
  atomically persists a new window and remote key before re-registering the
  same script. Empty keys preserve fresh allocation. Concurrent callers on one
  key serialize on `RPCServer.receiveScriptLocks`, a refcounted in-process
  keyed lock, so two retries never drive the same mailbox correlation ID at
  once; the entry is dropped when its last holder or waiter leaves.
  `NewReceiveScriptResponse.expires_at_unix_s` reports the absolute indexer
  registration expiry on both fresh and replayed paths.
- `SignCreditAccountAuthorization` (and the internal
  `RPCServer.SignCreditAccountAuth` behind it) signs a canonical swap
  credit-account request digest with the daemon identity key. It validates
  the digest (32 B), nonce (32 B), and account key (33 B compressed) lengths,
  **requires `account_pubkey` to equal this daemon's own identity key** —
  the daemon signs only for itself, and a mismatch is `InvalidArgument`, not
  `Internal` — and bounds the requested lifetime to
  `(now, now + swaprpc.CreditAccountMaxAuthTTL]`. It is granted under the
  `swap:write` macaroon entity alongside `SignOutSwapHtlcAck`.
- Under `js && wasm`, `ensureDataDir` is no longer an unconditional no-op: it
  calls `os.MkdirAll` and treats only `ENOSYS` (the browser's stub fs) as
  "no filesystem here". Every other error is returned, so a Node host given
  an unwritable path fails at startup rather than at the first database open.
- `OORLimitsConfig.MaxMailboxScriptBytes` must be at least
  `minOORMailboxScriptBytes = 34` (P2TR script length); validated in
  `Config.Validate()`.
- `Config.EagerRoundJoin` defaults via `defaultEagerRoundJoin()`: `false` on
  the standalone build, `true` under the `wavewalletrpc` build tag.
- `registerOOREventRoutes` checks for a typed `*oorpb.SubmitRejectedError`
  before the generic error path, so an OOR rejection drives an
  `OutboxErrorEvent` instead of an `Adapt` error that would stall the
  serverconn ingress cursor on the offending envelope (`server.go`).
  `oorRejectRetry` classifies the event's `Retryable` flag: it is `false`
  (terminal) for every typed reject except the two transient codes,
  `OOR_REJECT_INPUT_NOT_SPENDABLE` (the operator has not yet caught up to the
  input's commitment confirmation) and `OOR_REJECT_USER_BALANCE` (the
  recipient mailbox is over its balance cap), which re-drive the submit after
  `oorTransientRejectRetryDelay`. That retry is now bounded: the FSM's
  `AwaitingSubmitAccepted` transition (`handleSubmitOutboxError` in
  `oor/transitions.go`) gives up terminally once the cumulative retry window
  exceeds `OORConfig.MaxTransientSubmitRetry` (default 1h), persisting the
  window start (`FirstRejectUnixNanos`) in the outgoing snapshot (version 5)
  so the bound survives restarts.
- `operatorTermsFromResponse` and daemon `GetInfo` must preserve
  `FreeRefreshWindowBlocks` end to end.
- `RPCServer.OperatorVTXOFloor` refreshes authenticated operator terms for
  each credit materialization decision and bounds that refresh with
  `operatorTermsRefreshTimeout` (30 s). Its callers use daemon/actor lifetime
  contexts, so removing this local timeout can park boot reconciliation or a
  settled-receive actor turn forever on a stalled operator.
- `deriveIdentityKeyEarly` publishes `clientKeyDesc` before mailbox bootstrap.
  `GetInfo` reuses this descriptor instead of deriving the same key for every
  status request because btcwallet-backed derivation opens a wallet database
  write transaction. Before the descriptor is available, `GetInfo` keeps its
  pre-initialization fallback. All descriptor reads and startup writes go
  through `loadClientKeyDesc` and `storeClientKeyDesc` because `GetInfo` is
  callable while startup is publishing the value.
- The VTXO manager reads `FreeRefreshWindowBlocks` from the latest cached
  operator terms on each expiry check. It delays automatic refresh to the
  window boundary only when the local dynamic critical threshold plus retry
  buffer remains intact. When that cached boundary fires, it fetches a fresh
  `GetInfo` snapshot and rechecks the window before reserving the input.

## Deep Docs

- [docs/daemon_cli_guide.md](../docs/daemon_cli_guide.md) — Installation,
  configuration, CLI reference.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
