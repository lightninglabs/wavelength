# darepod

## Purpose

Top-level daemon orchestrator that wires wallet backend, mailbox transport,
chain backend, database, and all domain actors into a running system with a
gRPC API.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/darepo-client/darepod.<Symbol>`.

### Server & Configuration

- `Server` — main daemon. Owns wallet, DB, chainsource actor, gRPC
  server, and `ActorSystem`. Caches `localMailboxID` (pubkey-derived),
  `authSigHex` (Schnorr auth) and a single `clk` (`clock.Clock`) that
  all sub-stores share for deterministic time injection.
- `RPCServer` — implements gRPC `DaemonService`. Holds an in-memory
  `customInputLocks` map (guarded by `customInputLocksMu`) that
  reserves custom OOR input outpoints for the duration of a `SendOOR`
  call.
- `Config` — daemon configuration. Notable fields:
  `MailboxEdgeFactory` (test transport interception),
  `PackageSubmitter chainbackends.PackageSubmitter` (v3 CPFP submitter
  injected by the harness via `BitcoindPackageSubmitter` and by
  `cmd/darepod` from `bitcoind.{host,user,pass}` flags; not
  serialized), optional `Unroll *UnrollConfig`, and `MaxOperatorFeeSat
  int64` cap fed into `ClientEnvironment.MaxOperatorFee`. Under the
  #270 seal-time fee handshake every server-issued `JoinRoundQuote`
  is compared against this cap; `Config.Validate()` fails closed when
  the value is non-positive. CLI flag `--maxoperatorfeesat`;
  `DefaultMaxOperatorFeeSat` is a generous default.
- `UnrollConfig` — `BumpAfterBlocks int32` (fee-bump cadence; zero →
  default 6), `MaxFeeRateSatPerVByte int64` (cap fed to both
  `txconfirm` and the unroll registry; zero → each subsystem's
  default). Surfaced via `unrollMaxFeeRate()`.
- `OORConfig` / `OORLimitsConfig` — incoming receive safety caps
  (`MaxCheckpoints`, `MaxVTXOMatches`, `MaxMailboxItems`,
  `MaxMailboxScriptBytes`). `Config.OORReceiveLimits()` normalizes
  into `oor.ReceiveLimits`.
- `WalletState` — `None` / `Locked` / `Ready`.

### RPC Handlers

- `Board` — non-blocking; delegates to wallet actor.
- `GetRound` / `ListRounds` — round operation status. Live rounds come
  from the round actor; persisted rounds from SQL summaries. Live
  rounds surface `commitment_txid` once the FSM reaches
  `CommitmentTxReceived` (recovered from the state's `TxID` or the
  embedded commitment PSBT for MuSig2 phases) plus the per-owner
  expected VTXO amounts, filtered by `IsOwner=true` so other clients'
  outputs don't inflate the local view (helper: `liveRoundDetails`).
- `GetOORSession` / `ListOORSessions` — pending and failed from OOR
  actor `ListSessionsRequest`; completed from persisted artifacts.
  Actor state is authoritative when both views exist.
- `SendVTXO` — in-round directed sends. Validates recipients (count
  cap, positive and `MaxSatoshi`-bounded amounts, overflow-safe sum),
  resolves destinations via `resolveRecipientOutput`, delegates to
  the wallet actor.
- `resolveRecipientOutput` — extracts pkScript and client pubkey from
  an `Output` proto oneof (pubkey or address). Taproot-only.
- `ListVTXOs` — paginated VTXO inventory. When called with
  `VTXO_STATUS_PENDING_ROUND`, branches to `listPendingRoundVTXOs`
  which bypasses `s.vtxoStore` and projects synthetic VTXOs (amount
  + round id + commitment txid; outpoint deliberately empty) from
  each live round FSM via `queryRoundStates`. Rounds already in
  `ROUND_STATE_CONFIRMED` are skipped so the store-backed
  `VTXO_STATUS_LIVE` rows don't double-report.
- `Unroll` / `GetUnrollStatus` — manual unilateral-exit RPCs. `Unroll`
  short-circuits with `Created=false` when the VTXO is already in
  `VTXOStatusUnilateralExit`; else asks the VTXO manager
  `ForceUnrollRequest{Reason: "manual RPC request"}`. Uses
  `manualUnrollAdmissionTimeout`-bounded context derived via
  `context.WithoutCancel(ctx)` so CLI disconnect doesn't cancel the
  daemon-local handoff. `GetUnrollStatus` is read-through: prefers
  live registry via `queryUnrollRegistry`, falls back to
  `db.UnilateralExitPersistenceStore.GetJob`. Returns `Found=false`
  (not error) when neither layer has a record. With `Detailed=true`
  it additionally calls `enrichUnrollDetail` (live path) or
  `enrichExitFees` directly (persisted-only path) to fill a
  human `PhaseDetail` line (`unrollPhaseDetail`) and an
  `UnrollFees` breakdown (CPFP/sweep/total/net plus `SpentSoFarSat`,
  computed by `spentSoFarSat`) projected from the persisted
  descriptor via `unroll.PlanExitFunding` — the same cost model
  `GetExitPlan` and `ExitSummary` use. Both enrichment helpers are
  best-effort: a missing live actor, unloaded planner, or
  descriptor/fee-estimate failure just leaves the field unset
  rather than failing the probe.
- `ExitSummary` — wallet-wide portfolio of every non-terminal
  unilateral-exit job (`ueStore.ListNonTerminalJobs`), returning
  `ExitSummaryResult{Entries []ExitSummaryEntry, TotalExits,
  TotalVTXOAmountSat, TotalEstFeeSat, TotalEstNetRecoveredSat}`.
  Deliberately cheap: it does not query live actors, projecting each
  entry's amount/fee/net-recovered fields from the persisted VTXO
  descriptor with a nil lineage (approximated from `ChainDepth`) and
  a zero wallet snapshot via the shared `unroll.PlanExitFunding`
  model (`exitSummaryEntry`). A fee-estimate failure degrades to a
  zero rate (zeroing only the fee columns) instead of failing the
  call; a per-outpoint descriptor lookup failure yields a phase-only
  entry with zeroed amounts instead of failing the whole summary.
- `unrollPhaseToProto` / `unrollJobStatusToProto` — dual mappers from
  live `unroll.Phase` and persisted `db.UnilateralExitJobStatus` to
  the same proto. `PhaseSweepBroadcast` and `PhaseSweepConfirmation`
  both project to `UNROLL_JOB_STATUS_SWEEPING`.
- `EstimateFee` / `GetFeeHistory` — operator fee surface.
  `EstimateFee` proxies to the operator's `EstimateFee` over the
  direct gRPC connection (`s.serverConn`); no local caching.
  `GetFeeHistory` reads through
  `s.ledgerStore.ListLedgerEntriesWithFeesTotal` for page +
  cumulative-total consistency.
- `ListTransactions` — newest-first unified history from ledger +
  sweep DBs. Accepts `type` filter, optional time range, `limit`
  (cap 1000), `offset` (clamped to `math.MaxInt32`). Delegates to
  `ledgerStore.ListTransactionHistory` and projects via
  `transactionHistoryRowToProto`.
- `proxyUpstreamError(err, msg)` — gRPC-safety helper preserving
  upstream codes while stripping operator-side text. Errors without a
  status map to `codes.Unavailable`.
- `quoteOperatorFee` — internal helper asking the operator's
  `ArkService.EstimateFee` via direct gRPC. Returns
  `codes.Unavailable` when `serverConn` is nil (degraded mode) so
  callers can distinguish transient from permanent.
- `SweepBoardingUTXOs` — sweeps CSV-mature boarding UTXOs back to the
  wallet. Resolves candidates (explicit outpoints or all
  confirmed/failed/expired intents), estimates fee, builds and signs
  an aggregate tx via `buildBoardingSweepTx`, persists, broadcasts,
  and wakes `boardingSweepWatcher`. Returns preview when
  `broadcast=false`.
- `ListBoardingSweeps` — paginated persisted aggregate sweeps with
  optional status filter and cursor-based pagination.
- `ArmVHTLCRecovery` — persists a dormant vHTLC on-chain recovery job (armed
  state). The job remains dormant until `EscalateVHTLCRecovery` is called.
  Idempotent on `request_id`.
- `EscalateVHTLCRecovery` — transitions an armed job into active unroll by
  calling `coordinator.Service.EscalateRecovery`. Triggers
  `TargetMaterializer.EnsureRecoveryTarget` before admitting the target to
  the unroll registry.
- `CancelVHTLCRecovery` — marks a recovery job cancelled (cooperative
  settlement or explicit operator action).
- `StatusVHTLCRecovery` — returns the current recovery row joined with live
  unroll status for the target outpoint.
- `SendOnChain` — RPC handler delegating to the wallet actor's
  `SendOnChainRequest`. Routes through coin selection, leave output
  construction, and eager round join. Supports bounded and sweep-all modes.

### Adapters & Helpers

- `serverDurableUnaryBuilder` — implements
  `serverconn.DurableUnaryRequestBuilder` via the indexer client with
  proof-of-control credentials.
- `IndexerProofKey` — derives the fixed wallet key for a given key
  locator; returns an `indexer.SchnorrSigner` backed by the proof-key
  backend.
- `NewOwnedReceiveScriptSigner` — indexer signer that resolves the
  wallet key for any persisted owned receive script, then delegates
  to the backend-specific signer.
- `ownedScriptCheckerAdapter` / `ownedScriptRegistrarAdapter` /
  `ownedScriptLookupAdapter` — wrap `db.OORArtifactPersistenceStore`
  to satisfy `round.OwnedScriptChecker` /
  `round.OwnedScriptRegistrar` / `vtxo.OwnedScriptLookup`. The
  checker uses `context.WithoutCancel` so confirmation-time ownership
  survives FSM shutdown; returns `false` on `sql.ErrNoRows`. The
  registrar persists pkScripts as `OwnedReceiveScriptSourceWallet`
  with the operator pubkey and VTXO exit delay from `OperatorTerms`.
- `EnsureDefaultOORReceiveScript` / `CreateOORReceiveScript` —
  receive-key lifecycle: derive, register with indexer
  (proof-of-control), persist ownership record.
- `ResolveIncomingMetadataFromIndexer` — resolves authoritative VTXO
  lineage metadata from `ListVTXOsByScripts`.
- Ancestry conversion lives in
  [`vtxo.AncestryFromRPC`](../vtxo/incoming_ancestry.go); the
  darepod-local copy that previously lived here was deleted when the
  OOR and in-round receive paths both converged on the shared `vtxo`
  helper. `vtxo.MaxAncestryPaths = 64` is the shared cap.
- `lndUnrollWallet` / `lwUnrollWallet` / `btcwUnrollWallet` —
  backend-specific adapters satisfying both `txconfirm.Wallet`
  (`ListUnspent`/`NewWalletPkScript`/`FinalizePsbt`/`LeaseOutput`/
  `ReleaseOutput`) and `unroll.SweepWallet`. LND forwards to the
  `BoardingBackend`; lwwallet/btcwallet paths reach into `BtcWallet`
  directly, reinterpreting `wallet.LockID` as `wtxmgr.LockID` via
  direct `[32]byte` cast so leases round-trip across restart.
- `reserveCustomInputs` (on `RPCServer`) — atomically claims every
  custom OOR outpoint for a `SendOOR` call. Returns a release
  function (typically deferred).
- `autoRefreshFeeQuoter` — wires `vtxo.RefreshFeeQuoter` into every
  VTXO actor. Advisory under #270: the closure's return value
  populates `RefreshVTXORequest.OperatorFee` for observability but
  is not written to the intent. Falls back to
  `terms.MinOperatorFee` when unreachable.
- `boardingSweepWatcher` — daemon-owned background watcher: resumes
  pending sweeps on startup, rate-limited rebroadcasts, registers
  spend notifications per input, marks inputs spent on confirmation.
  Started by `startBoardingSweepWatcher` on wallet unlock;
  idempotent.
- `vhtlcRecoveryTargetMaterializer` — darepod adapter implementing
  `coordinator.TargetMaterializer`. Binds vHTLC recovery rows to local OOR
  packages and VTXO descriptors so the generic unroll subsystem can assemble
  lineage and watch the target without swap-specific knowledge.
- `boardingSweepTx` / `buildBoardingSweepTx` — constructs and signs
  one aggregate timeout-path sweep tx. Iterates the weight estimate
  up to three times until `SerializeSize` converges so `fee`/`txid`
  are accurate. Validates `defaultBoardingSweepMaxFeePercent = 25%`
  and `defaultBoardingSweepMaxInputs = 100`.
- `deriveIdentityKeyEarly` — derives the client's secp256k1 identity
  key from LND or lwwallet before mailbox transport starts.
- `signMailboxAuth` — Schnorr auth. LND uses the tagged Schnorr
  signing RPC (`withSchnorrTag`); lwwallet signs locally via
  `serverconn.SignMailboxAuth`.
- `fetchOperatorPubKeyDirect` — fetches operator pubkey via direct
  gRPC `GetInfo` before the mailbox runtime starts.
- `initLedgerActor` — constructs `ledger.LedgerActor` with both
  `LedgerStoreDB` and `UTXOAuditStoreDB`, registers under
  `ledger.ServiceKeyName`, stashes `LedgerStoreDB` on the `Server`
  for RPC reads. Called after DB ready but before wallet unlock.
- `initUnrollSubsystem` — wires the unilateral-exit runtime during
  `startWalletDependentActors` (step 12, before `initOORActor`).
  Builds a backend adapter, registers the shared `TxBroadcasterActor`
  under `"txconfirm"`, constructs `UnrollRegistryActor` with the
  persistence store, `LocalProofAssembler`, shared `txConfirmRef`,
  and wallet, then calls `RestoreNonTerminal(ctx)`. Builds a
  `MapInputRef` translating `vtxo.ExpiringNotification` →
  `unroll.EnsureUnrollRequest{Trigger: TriggerCriticalExpiry}` and
  hands it to `lazyChainResolver.Set`.
- `unrollMaxFeeRate` — `cfg.Unroll.MaxFeeRateSatPerVByte` if
  positive, else zero (each downstream uses its own default).

### Test Hooks (NOT for production)

- `TriggerRoundRegistration` — injects an `IntentRequested` event
  into the round actor; backs `JoinNextRound` RPC and the harness
  registration hook. Uses `context.WithoutCancel` on `Ask` so the
  caller's ctx doesn't propagate into the FSM's forfeit-VTXO lookup;
  keeps original ctx on `Await`.
- `GetStoredVTXO` — harness-only accessor returning a persisted
  `vtxo.Descriptor` for an outpoint directly from the VTXO store.
- `GetVTXOLineageTx` / `VTXOLineageEntry` — harness-only accessor
  returning one lineage tx plus the outpoints of its parent txs.
  Walked by recursing on each parent outpoint until
  `OnChainRoot=true`. Implemented on top of the same
  `unroll.LocalProofAssembler`, but routed through the terminal-
  tolerant `EnsureProofForHarness` entry point so the lineage of an
  already-spent / forfeited VTXO stays walkable. The field type
  `harnessProofAssembler` is a 1-method local interface exposing
  ONLY the terminal-tolerant entry point so production code paths
  cannot reach `EnsureProof` through this seam.
- `NewWalletAddress` / `ListWalletUnspent`
  (`wallet_testhooks.go`) — backend-agnostic harness helpers
  returning a fresh P2TR address and the current confirmed UTXO set.

## Relationships

- **Depends on**: `baselib/actor`, `btcwbackend`, `chainbackends`,
  `chainsource`, `lib/actormsg`, `db`, `ledger`, `round`, `txconfirm`,
  `unroll`, `vtxo`, `wallet`, `walletcore`, `oor`, `serverconn`,
  `indexer`, `arkrpc`, `lndbackend`, `harness` (bitcoind package
  submitter wiring in `cmd/darepod`), `fraud`, `gateway`,
  `rpc/restclient`, `vhtlcrecovery`, `vhtlcrecovery/coordinator`,
  `vhtlcrecovery/unrollpolicy`.
- **Depended on by**: `cmd/darepod`.

## Invariants

- Server owns `ActorSystem` lifetime; `Server.run` registers a
  deferred `actorSystem.Shutdown()` **before** the deferred
  `db.Close()` so all actor DB transactions drain before the
  connection pool tears down. Without this ordering, in-flight actor
  lease loops produce "sql: database is closed" warnings at the tail
  of every itest.
- Wallet transitions `None → Locked → Ready` (or direct to Ready if
  seed provided).
- Three wallet modes: LND-backed, lightweight (`lwwallet`), or
  neutrino-backed (`btcwallet` via `btcwbackend`).
- Mailbox IDs are derived from identity pubkeys (via
  `serverconn.PubKeyMailboxID`), not config strings. The operator's
  remote mailbox ID is fetched via direct gRPC before the mailbox
  runtime starts.
- Auth headers (Schnorr signature) are injected into all outbound
  envelopes including response envelopes in `handleInboundRPC`.
- TLS client cert generation is skipped in insecure mode.
- Per-subsystem logging: configurable log writer, no global mutable
  loggers.
- All sub-stores share the single `s.clk` clock assigned at
  `NewServer`. **New code must not call `clock.NewDefaultClock()` in
  `init*` methods** — use `s.clk`.
- `SendVTXO` enforces `maxRecipients = 256` (TODO #241), rejects
  per-recipient amounts outside `(0, MaxSatoshi]`, uses
  overflow-safe accumulation. Wallet-side `handleSendVTXOs` repeats
  these checks as defense-in-depth.
- `SendOOR` with custom inputs serializes concurrent calls on the
  same outpoints via `reserveCustomInputs`. Custom inputs lock for
  the RPC lifetime; release is deferred on both success and failure.
- `BuildCustomTransferInputs` validates (a) the caller-supplied
  policy template compiles to the provided pkScript
  (`PolicyTemplate.MatchesPkScript`), and (b) the spend path's
  control block commits to the same pkScript
  (`SpendPath.VerifyBindsToPkScript`). Together these prevent a
  caller from obtaining signatures for an unrelated tapscript by
  claiming a different output's policy template.
- `ListRounds` splits pending (in-memory from actor) and persisted
  (SQL with cursor pagination).
- Actor startup order: VTXO manager starts BEFORE round actor and
  OOR actor so the manager ref is available for both. The round
  actor ref in the VTXO manager is lazy (service-key-based, resolved
  at Tell time).
- `mapRoundVTXOManagerMsg` bridges `round.VTXOManagerMsg` →
  `vtxo.ManagerMsg` via `MapInputRef`. Compile-time assertions
  enforce that all `round.VTXOManagerMsg` implementors satisfy
  `vtxo.ManagerMsg`.
- OOR receive-key is derived once at startup via
  `EnsureDefaultOORReceiveScript` and persisted for restart-safe
  re-registration. The `DurableUnaryBuilder` is wired through
  `serverconn.ConnectorConfig` so all indexer queries flow through
  the durable transport.
- The OOR artifact store backs three round/vtxo abstractions
  (`OwnedScriptChecker`, `OwnedScriptRegistrar`, `OwnedScriptLookup`).
  One logical "owned receive scripts" table; all ownership questions
  resolve through it.
- The incoming VTXO handler actor is registered under
  `vtxo.IncomingVTXOServiceKey()` during `initOORActor`. Mailbox
  route `MethodIncomingVTXO` decodes `arkrpc.IncomingVTXOEvent` push
  notifications and dispatches them.
- Every producer actor (`wallet.NewArk`, `round.RoundClientConfig`,
  `vtxo.ManagerConfig`, `oor.ClientActorCfg`) is wired with
  `fn.Some(ledger.NewSink(s.actorSystem))`. `wallet.NewArk` takes
  the sink as a required constructor argument so every call site
  makes an explicit emission choice; test harnesses pass
  `fn.None[ledger.Sink]()`.
- `EstimateFee` and `GetFeeHistory` route upstream errors through
  `proxyUpstreamError` to preserve gRPC codes and strip operator-side
  detail. `GetFeeHistory` validates request bounds locally (limit
  positive, offset within `int32` range) before hitting the DB.
- In btcwallet mode, neutrino is pre-started before seed availability
  so P2P sync proceeds in parallel. `neutrinoSvc` uses `fn.Option`
  and is reused by `startBtcwallet` via `NewWithNeutrino`.
- The neutrino sync-wait goroutine polls indefinitely (no timeout)
  with 30s progress logging — avoids leaving the wallet permanently
  unready.
- `ensureRoundExists` in `db/vtxo_store.go` uses check-then-insert
  (not upsert) because `InsertRound`'s `ON CONFLICT DO UPDATE` would
  overwrite richer round state.
- **Unroll subsystem ordering**: wired strictly AFTER the VTXO
  manager but BEFORE the OOR actor. The VTXO manager is created with
  a `vtxo.LazyChainResolver` placeholder so VTXO actors spawned
  during manager construction hold a stable ref;
  `initUnrollSubsystem` later calls `lazyChainResolver.Set(...)`.
  Any code that also needs this seam must run AFTER
  `initUnrollSubsystem` or it will see an unset target.
- `initUnrollSubsystem` creates its own `dbStore` + `vtxoStore` to
  decouple the unroll store lifecycle from the VTXO manager's; the
  persisted `s.ueStore` is reused by the `GetUnrollStatus` fallback
  so terminal jobs remain queryable after registry eviction.
- `Server.run` registers a deferred `s.unrollRegistry.Stop()` during
  startup so the registry's durable persist writer drains before
  actor-system shutdown.
- `registerOOREventRoutes` checks for `*oorpb.SubmitRejectedError`
  before a generic error check on the submit-package response. A
  typed server-side rejection (e.g. `OOR_REJECT_LINEAGE_TOO_LARGE`)
  becomes an `oor.OutboxErrorEvent{Retryable: false}` rather than
  surfacing as an Adapt error — prevents the serverconn ingress
  dispatcher from stalling the cursor on a sticky rejection.
- `Unroll` and `GetUnrollStatus` return `codes.Unavailable` (not
  `Internal`) when subsystem refs are not yet set, so clients can
  retry rather than treating it as permanent failure.
- `SweepBoardingUTXOs` always persists the sweep record before
  broadcasting; on broadcast failure the record is marked failed so
  the watcher does not rebroadcast. Spend watcher is refreshed via
  `getBoardingSweepWatcher().Refresh` (using
  `context.WithoutCancel`) immediately after a successful broadcast.
- `boardingSweepWatcher` uses two cancellation scopes: `w.ctx` for
  spend registration (watcher lifetime, survives CLI disconnect) and
  the per-refresh `ctx` for rebroadcast RPCs.
- `OORConfig.OOR.Limits.MaxMailboxScriptBytes` must be at least
  `minOORMailboxScriptBytes = 34` (P2TR script length); validated
  during `Config.Validate()`.
- `spentSoFarSat` prorates the CPFP total over broadcast (confirmed +
  in-flight) proof txs by multiplying by the broadcast count **before**
  dividing by `progress.TotalTxs` — dividing per-child first truncates
  to zero whenever the CPFP total is smaller than the tx count.
  Numerator and denominator must count the same proof-graph universe
  (deduped node count), since it's an estimate, not accounting: a
  confirmed proof tx may be a shared ancestor another party paid the
  CPFP for. The sweep leg is added only once `sweepBuilt` is true.
- `unrollPhaseDetail`'s materializing line clamps its 1-based
  `CurrentLayer+1` display to `TotalLayers`: the frontier layer
  collapses to `TotalLayers` once every proof node confirms, so
  without the clamp a job read as `MATERIALIZING` right after the
  frontier collapses would render "layer N+1 of N".
- `Config.EagerRoundJoin` is seeded by build-tag-aware
  `defaultEagerRoundJoin()`: `false` on the standalone non-walletdkrpc
  build, `true` under the `walletdkrpc` tag (both `cmd/darepod` and
  `sdk/walletdk` embedded paths). The `--eagerroundjoin` flag
  inherits this default so viper precedence overrides it naturally
  without `IsSet` probing. `sdk/walletdk` exposes the disable knob
  via `WithEagerRoundJoinDisabled()`.

## Deep Docs

- [docs/daemon_cli_guide.md](../docs/daemon_cli_guide.md) —
  Installation, configuration, CLI reference.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
