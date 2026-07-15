# waved

## Purpose

Top-level daemon orchestrator that wires the wallet backend, mailbox
transport, chain backend, database, and all domain actors into a running
system with a gRPC API.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/wavelength/waved.<Symbol>`.

- `Server` — main daemon. Owns the wallet, DB, chainsource actor, gRPC
  server, and `ActorSystem`. Caches `localMailboxID` (pubkey-derived),
  `authSigHex` (Schnorr auth), and a single `clk` (`clock.Clock`) shared by
  all sub-stores for deterministic time injection.
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
  `vhtlcrecovery`, `vhtlcrecovery/coordinator`, `vhtlcrecovery/unrollpolicy`.
- **Depended on by**: `cmd/waved`.

## Invariants

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
- `SendVTXO` enforces `maxRecipients = 256`, rejects per-recipient amounts
  outside `(0, MaxSatoshi]`, and uses overflow-safe summation; the wallet
  actor repeats these checks as defense-in-depth.
- `SendOOR` with custom inputs serializes concurrent calls on the same
  outpoints via `reserveCustomInputs`; the release function is deferred on
  both success and failure.
- `Unroll` / `GetUnrollStatus` return `codes.Unavailable` (not `Internal`)
  when the unroll subsystem refs are not yet set, so clients can retry.
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
  (terminal) for every typed reject except `OOR_REJECT_INPUT_NOT_SPENDABLE`,
  which is transient (the operator has not yet caught up to the input's
  commitment confirmation) and so re-drives the submit after
  `oorInputNotSpendableRetryDelay`.

## Deep Docs

- [docs/daemon_cli_guide.md](../docs/daemon_cli_guide.md) — Installation,
  configuration, CLI reference.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
