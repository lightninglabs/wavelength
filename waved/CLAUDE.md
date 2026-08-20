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
- `UnrollConfig` / `OORConfig` — subsystem tunables; see `Config.Validate()`
  for the invariants each enforces.

## Relationships

- **Depends on**: `baselib/actor`, `btcwbackend`, `chainbackends`,
  `chainsource`, `lib/actormsg`, `db`, `ledger`, `round`, `txconfirm`,
  `unroll`, `vtxo`, `wallet`, `walletcore`, `oor`, `serverconn`, `indexer`,
  `arkrpc`, `lndbackend`, `fraud`, `gateway`, `rpc/restclient`,
  `vhtlcrecovery`, `vhtlcrecovery/coordinator`, `vhtlcrecovery/unrollpolicy`,
  `tapassets` (the only importer: onboarding, OOR preparation, claim).
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
- `BoardTaprootAsset` funds its round's fee from Bitcoin the client already
  holds in Ark (`taproot_asset_round_fee.go`). An asset VTXO request is
  `FixedAmount`, so a round carrying only one gives the operator no output to
  stamp the seal-time residual on and `resolveChangeDesignation` rejects the
  whole intent. `fundAssetRoundFee` therefore refreshes one live Bitcoin VTXO
  into the same assembling round — reusing the wallet's `RefreshVTXOsRequest`
  rather than composing forfeits itself — and the residual returns as change.
  Selection is smallest-sufficient over `fee + MinVTXOAmountFloor()` with an
  outpoint tie-break, so a large coin is never churned for a small fee. It is
  skipped when the client's intents for that round already include a non-fixed
  output it owns (`hasFeeFundingSlot`), which is what keeps the same-round
  Bitcoin-boarding flow working unchanged. The check runs *before* the
  boarding is persisted, so a wallet with no spendable Bitcoin fails the RPC
  with `FailedPrecondition` naming the fix instead of assembling a round the
  operator rejects at seal. Nothing on the operator changes: a Bitcoin forfeit
  carries zero asset units, so `validateAssetForfeitBalance` returns early and
  `planAssetRound`'s boarding/refresh mix guard never sees it.
- `BoardTaprootAsset` **triggers the round join itself**
  (`joinAssetBoardingRound` → `TriggerRoundRegistration`, an
  `IntentRequested` notification to the round actor). `EagerRoundJoin`
  defaults to `false` on the standalone build, so registering the
  boarding disclosure and the asset VTXO request only *queues* intents:
  the round actor then waits for a `JoinNextRound` that a one-shot
  boarding caller never sends, and the intents sit parked forever. The
  nudge also runs on the `already_boarded` replay path, so a crash
  between registration and join is recoverable. `round.ErrNoPendingRound`
  is swallowed — a join is already in flight, or an earlier call
  committed the intents.
- The boarded output carries `terms.BoardingExitDelay`, re-read from live
  operator terms on every replay rather than persisted in
  `taprootAssetBoardRequest`. Round admission rejects the shorter VTXO
  delay, which leaves the operator too little margin on an input it has
  to be able to forfeit.
- A `BoardTaprootAsset` replay reports `already_boarded` and returns
  before confirmation gathering and before `fundAssetRoundFee`, so
  `value_sat` and the fee-funding fields stay zero on that path. The
  replay is detected against `FetchBoardingIntentOutpoints`, not against
  the journal, because the journal alone cannot distinguish "onboarded"
  from "onboarded and already registered".
- **Asset OOR sends are operator-funded end to end.** `SendOOR` with a
  `taproot_asset` intent rejects `dry_run`, custom inputs, a missing
  idempotency key, more than one recipient, a non-zero recipient
  `amount_sat`, and a non-zero `asset_change_carrier_value_sat`: the
  daemon stamps every new asset leaf at `MinVTXOAmountFloor()` out of a
  leased operator float, so a caller-supplied carrier can only
  contradict it. A sender needs no Bitcoin at all for an asset send.
- Daemon-side input selection (`selectTaprootAssetOORInputs`) runs when
  `input_vtxo_outpoint` is empty: live candidates filtered to the exact
  `asset_ref`, sorted descending by units with an outpoint tie-break,
  then the **smallest sufficient single VTXO** if one exists (it
  preserves the larger leaves), else largest-first accumulation. Hitting
  `oor.MaxTaprootAssetInputs` (8) is a consolidate-first
  `FailedPrecondition`, not a silent partial send. In selection mode the
  caller's `asset_amount` is reinterpreted as the amount to send and
  becomes `RecipientAssetAmount` when it is short of the selected total.
- `orderAssetSelectedOutpoints` re-imposes the intent's order on whatever
  the wallet's lock returned, because the transition input order is
  consensus-relevant: the spine must be transition input 0. It returns a
  copy of the required set, so a Bitcoin filler input the wallet added
  cannot slip in, and a count or membership mismatch is `Internal`.
- The carrier float is leased **before any wallet reservation**
  (`leaseOORCarrier`, `requiredSat = MinVTXOAmountFloor() *
  NewAssetLeafCount()`), so a lease failure costs nothing.
  `BuildOperatorFundedTransferInput` binds the lease locally rather than
  trusting it: the policy template must match the returned pkScript,
  validate against the operator key, and name the `GetInfo`-advertised
  `oor_carrier_pubkey` as owner. The owner leaf is rebuilt as the
  float's own collab multisig, and no client key is stamped — the
  operator signs both legs, which is what `OperatorFunded` tells every
  signing site. There is **no release RPC**; an unused lease expires on
  its own.
- A resumed asset send must reuse its journaled lease. `assetResume`
  with a nil `Lease` is `Aborted` (reconciliation required), never a
  fresh lease, because a second float would double the operator's
  exposure for one transfer.
- `registerTaprootAssetChangeAliases` skips the recipient whose pkScript
  equals the lease pkScript: the operator's float residual is not wallet
  money and must not get an owned receive script.
- `ClaimTaprootAssetVTXO` is a **separate asset-aware spend**, not part
  of the generic exit. The unroll actor deliberately withholds the final
  sweep for an asset target (`resolveExitSpendPolicy` refuses when
  `desc.TaprootAssetRoot != nil`) because a plain sweep would spend the
  carrier as satoshis and destroy the asset commitment; the unroll has
  already put the composed output on chain under the owner's exit leaf,
  and the claim finishes the job after the exit delay matures.
- The claim gathers **every** lineage anchor's confirmation across the
  DAG, not just the spine: `CollectAssetProofPathAnchors` recurses
  through each merging step's co-input paths, and each anchor is matched
  against `assetLineageOutputs` (the ancestry tree paths) to supply the
  output script the chain backend needs beside the txid. Those
  confirmations upgrade the compact path into a confirmed proof file
  (`ConfirmProofFile`) before the composed exit leaf is spent into a
  fresh tapd-owned anchor, paying the claim's miner fee out of the
  carrier. The claim requires `VTXOStatusUnilateralExit`.
- **KNOWN GAP: an OOR-received asset VTXO cannot be claimed.** Both
  claim entry points (`Server.ClaimAssetVTXO` and
  `assetClaimConfirmations`) require a non-empty
  `Descriptor.TaprootAssetSealedPackage`, and only a round-created leaf
  persists one. Such a leaf can still be spent onward out of round, but
  after a unilateral exit its carrier is strandable value: the sweep is
  withheld and the claim refuses. Closing this means persisting the
  receiving side's package, not relaxing the check.
- Asset-bearing VTXOs are rejected from every Bitcoin-shaped flow
  (`rejectAssetBearingTargets` / `bitcoinOnlyOutpoints`): explicit
  `RefreshVTXOs`, `LeaveVTXOs`, and `SendOnChain` targets are
  `InvalidArgument`, sweep-all filters them out, and the exit preflight
  short-circuits, because an asset VTXO's worth is the units riding on
  it rather than its carrier satoshis.
- `ConfigureTaprootAssets` registration is lazy and at most once per
  `Config`: it appends a service registrar, so no authenticated tapd
  connection or journal is opened until the gRPC services start, and it
  is a no-op when a caller injected its own `TaprootAssetOORPreparer`
  (embedded consumers own that integration).
- Onboarding hard-requires the **LND** wallet backend
  (`signTaprootAssetOnboardingAnchor`), because the anchor's carrier
  satoshis, the asset-change output, and the miner fee all come from the
  same on-chain wallet tapd is backed by, and the PSBT is signed and
  finalized through lnd's `WalletKit`.
- The three asset RPCs (`OnboardTaprootAsset`, `BoardTaprootAsset`,
  `ClaimTaprootAssetVTXO`) are granted under `onchain:write`; the asset
  send rides `SendOOR` and stays under `oor:write`.
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
- [docs/taproot_assets_architecture.md](../docs/taproot_assets_architecture.md)
  — Client-side Taproot Assets architecture: onboarding, boarding and
  round fees, asset OOR sends with operator-funded carriers, exit and
  claim, and the known gaps.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
