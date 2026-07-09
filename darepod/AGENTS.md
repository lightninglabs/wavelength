# darepod

## Purpose

Top-level daemon orchestrator that wires the wallet backend, mailbox
transport, chain backend, database, and all domain actors into a running
system with a gRPC API.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/darepo-client/darepod.<Symbol>`.

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
- **Depended on by**: `cmd/darepod`.

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
- Boarding-sweep transaction construction, fee estimation, spend watching,
  and startup resumption live inside the **wallet actor**
  (`wallet.Ark.handleSweepBoardingUTXOs` / `handleResumeBoardingSweeps` in
  `wallet/boarding_sweep_actor.go` and `wallet/boarding_sweep.go`), not in
  darepod. `RPCServer.SweepBoardingUTXOs` only validates the request and
  `Ask`s the wallet actor; darepod supplies the boarding store
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
  the standalone build, `true` under the `walletdkrpc` build tag.
- `registerOOREventRoutes` checks for a typed `*oorpb.SubmitRejectedError`
  before the generic error path, so an OOR rejection drives a
  non-retryable `OutboxErrorEvent` instead of an `Adapt` error that would
  stall the serverconn ingress cursor on the offending envelope
  (`server.go`).

## Deep Docs

- [docs/daemon_cli_guide.md](../docs/daemon_cli_guide.md) — Installation,
  configuration, CLI reference.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
