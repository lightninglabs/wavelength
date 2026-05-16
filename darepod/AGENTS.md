# darepod

## Purpose

Top-level daemon orchestrator that wires wallet backend, mailbox transport,
chain backend, database, and all domain actors into a running system with a
gRPC API.

## Key Types

- `Server` — Main daemon owning wallet, DB, chainsource actor, gRPC server, and ActorSystem. Caches `localMailboxID` (pubkey-derived), `authSigHex` (Schnorr auth) and a single `clk` (`clock.Clock`) that all sub-stores share for deterministic time injection.
- `RPCServer` — Implements the gRPC `DaemonService` API (Board, ListRounds, WatchRounds, NewReceiveScript, SendVTXO, etc.). Includes test hooks for mailbox edge factory and round registration. Holds an in-memory `customInputLocks` map (guarded by `customInputLocksMu`) that reserves custom OOR input outpoints for the duration of a `SendOOR` call to prevent concurrent callers from double-signing the same custom input.
- `Config` — Daemon configuration (data dir, network, RPC host, wallet type,
  etc.). Includes `MailboxEdgeFactory` hook for test harness transport
  interception, `PackageSubmitter chainbackends.PackageSubmitter` for injecting
  a v3 CPFP package submitter, optional `Unroll *UnrollConfig` tuning, optional
  `Swap *SwapConfig` (swap subserver config), optional
  `SwapWallet *SwapWalletConfig` (wallet subserver config), and
  `MaxOperatorFeeSat int64` cap. New: `LogDirPath string` overrides the
  network-scoped log directory used by the CLI (`logdir` mapstructure key); when
  empty, logs go to `DataDir/logs/<network>`.
- `SwapConfig` — Swap subserver config. New fields: `SuppressResume bool`
  (programmatic; suppresses the swap subserver's own resume sweep so a higher
  layer can own the lifecycle) and `Backend SwapBackend` (set by
  `swapclientserver.Register` after the swap subserver wires; lets the walletrpc
  subserver drive `ResumePending` in-process).
- `SwapBackend` — Interface exposed by `swapclientserver` after `Register`:
  `ResumePending(ctx)`. Allows walletrpc to own the unified resume policy
  without dialing the daemon's gRPC server from inside the same process.
- `SwapWalletConfig` — Configuration for the optional walletrpc subserver
  (compiled in when both `walletrpc` + `swapruntime` build tags are present).
  Fields: `Deadline`, `DefaultListLimit`, `MaxListLimit`, `SubscribeBuffer`.
  Present in all builds so config files stay stable; fields are inert in default
  builds.
- `UnrollConfig` — Tuning for the unilateral-exit subsystem. Fields: `BumpAfterBlocks int32` (fee-bump cadence; zero → default 6) and `MaxFeeRateSatPerVByte int64` (cap fed to both `txconfirm` and the unroll registry; zero → each subsystem's own default). Surfaced on the `Server` via `unrollMaxFeeRate()` which gates non-positive values to zero.
- `TriggerRoundRegistration` — Test-hook method that injects a round registration event into the round actor (in `server_round_testhook.go`).
- `GetStoredVTXO` — Harness-only accessor that returns a persisted `vtxo.Descriptor` for a given outpoint directly from the daemon's VTXO store. Lets integration tests inspect partial unroll state without reaching into internal fields.
- `GetVTXOLineageTx` / `VTXOLineageEntry` — Harness-only accessor that returns one transaction in a VTXO's recovery lineage plus the outpoints of its parent txs. Callers walk the lineage by recursively calling with each returned parent outpoint until `OnChainRoot=true` (the parent is the on-chain batch tx). Implemented on top of the same `unroll.LocalProofAssembler` the unroll registry uses (stashed on `Server.proofAssembler` during `initUnrollSubsystem`), but routed through the assembler's terminal-tolerant `EnsureProofForHarness` entry point so the historical lineage of an already-spent or already-forfeited VTXO is still walkable — that is the load-bearing capability for fraud-response itests where the harness force-broadcasts lineage txs of a terminal VTXO to provoke server-side classification + response. The field type `harnessProofAssembler` is a 1-method local interface that exposes ONLY the terminal-tolerant entry point, so production code paths cannot reach `EnsureProof` through this seam — production proof assembly flows exclusively through the unroll registry's own `ProofAssembler` reference, which keeps the terminal-status guard in force. NOT for production use.
- `WalletState` — Enum (None/Locked/Ready) for wallet lifecycle.
- `WalletLifecycleState()` — Public `Server` method returning current
  `WalletState`. Surfaced via `GetInfo` RPC as `daemonrpc.WalletState` so
  callers can distinguish not-yet-created from locked from ready.
- `boardingSweepWatcher` / `resumeBoardingSweeps` / `replayPendingBoardRequest`
  — Boarding sweep subsystem moved from direct `darepod` handlers to the wallet
  actor. The daemon calls `resumeBoardingSweeps` (step 13) and
  `replayPendingBoardRequest` (step 13b) after txconfirm and the round-client
  actor register with the receptionist.
- `fraudWatcher` / `fraudWatcherRef` — Passive recipient fraud watcher wired
  during `initOORActor`. Arms spend watches on OOR VTXO ancestors and triggers
  unilateral exit via the unroll registry on fraud detection.
- `serverDurableUnaryBuilder` — Implements `serverconn.DurableUnaryRequestBuilder` by delegating to the indexer client with proof-of-control credentials.
- `IndexerProofKey` — Public server method that derives the fixed wallet key for a given key locator and returns an `indexer.SchnorrSigner` backed by the proof-key backend. Used by `EnsureDefaultOORReceiveScript` and the `serverDurableUnaryBuilder` to produce per-request proof-of-control signatures.
- `NewOwnedReceiveScriptSigner` — Indexer signer that resolves the wallet key for any persisted owned receive script, then delegates signing to the backend-specific signer.
- `ownedScriptCheckerAdapter` — Wraps `db.OORArtifactPersistenceStore` to satisfy `round.OwnedScriptChecker`. Uses `context.WithoutCancel` so the confirmation-time ownership lookup survives FSM shutdown. Returns `false` on `sql.ErrNoRows`.
- `ownedScriptRegistrarAdapter` — Wraps the same store to satisfy `round.OwnedScriptRegistrar`. Persists pkScripts as `OwnedReceiveScriptSourceWallet` with the operator pubkey and VTXO exit delay from `OperatorTerms`.
- `ownedScriptLookupAdapter` — Wraps the store to satisfy `vtxo.OwnedScriptLookup` for the incoming VTXO handler, converting `db.OwnedReceiveScriptRecord` to `vtxo.OwnedReceiveScript`.
- `EnsureDefaultOORReceiveScript` / `CreateOORReceiveScript` — Receive-key lifecycle: derive, register with indexer (proof-of-control), persist ownership record.
- `ResolveIncomingMetadataFromIndexer` — Resolves authoritative VTXO lineage metadata from the indexer's `ListVTXOsByScripts` response for incoming materialization.
- `SendVTXO` — RPC handler for in-round directed sends. Validates recipients (count cap, positive and `MaxSatoshi`-bounded amounts, overflow-safe sum), resolves destinations via `resolveRecipientOutput`, and delegates to the wallet actor.
- `resolveRecipientOutput` — Extracts pkScript and client pubkey from an `Output` proto oneof (pubkey or address). Enforces taproot-only for directed sends.
- `registerIncomingVTXOEventRoute` — Registers the `arkrpc.IncomingVTXOEvent` mailbox route under `MethodIncomingVTXO`, dispatching decoded events to the incoming VTXO handler actor via its service key.
- `GetRound` / `ListRounds` — RPC handlers for round operation status. Live
  rounds come from the round actor; persisted rounds come from SQL summaries
  including commitment txid, confirmation height, creation/update timestamps,
  locally known inputs, and created VTXOs.
- `GetOORSession` / `ListOORSessions` — RPC handlers for OOR operation status.
  Pending and failed sessions come from the OOR actor's `ListSessionsRequest`;
  completed sessions come from persisted OOR package artifacts. The merge keeps
  actor state authoritative when both live and persisted views exist.
- `initLedgerActor` — Constructs `ledger.LedgerActor` with both `db.NewLedgerStoreDB` (double-entry ledger) and `db.NewUTXOAuditStoreDB` (UTXO audit log) as stores, starts it, registers it with the actor system under `ledger.ServiceKeyName`, and stashes the `LedgerStoreDB` on the `Server` as `s.ledgerStore` so the RPC layer can read paginated history without going through the actor mailbox. Called in `run` after the DB and delivery store are ready but before wallet unlock, since the actor does not depend on wallet state.
- `EstimateFee` — RPC handler that proxies to the operator's `EstimateFee` over the direct gRPC connection (`s.serverConn`, reused from `fetchOperatorTerms`). No local caching: the operator's reply reflects live treasury utilization, so callers always see fresh numbers.
- `GetFeeHistory` — RPC handler that reads through `s.ledgerStore.ListLedgerEntriesWithFeesTotal` for mutual consistency between the page and the cumulative operator-fees-paid total. Validates limit/offset bounds (offset clamped to `math.MaxInt32`) and converts sqlc rows to proto `FeeHistoryEntry` with debit/credit accounts, round_id, session_id, and event_type verbatim via `ledgerEntryToProto`.
- `ListTransactions` — RPC handler that reads a unified newest-first transaction history page from the ledger and sweep databases. Accepts `type` filter (`boarding`, `round`, `oor`, `sweep`), optional `from_unix_s`/`to_unix_s` timestamp range, `limit` (capped at 1000), and `offset` (clamped to `math.MaxInt32`). Delegates to `ledgerStore.ListTransactionHistory` and converts each sqlc row via `transactionHistoryRowToProto`.
- `proxyUpstreamError(err, msg) error` — gRPC-safety helper that extracts the upstream gRPC status, preserves the code, and returns a new status carrying a generic RPC-scoped message. Errors without a status map to `codes.Unavailable` so clients can retry. Used by `EstimateFee` and `GetFeeHistory` to avoid collapsing codes to `Unknown` and leaking operator-side error text across the daemon→client boundary.
- `quoteOperatorFee` — Internal helper that asks the operator's `ArkService.EstimateFee` via direct gRPC and returns `TotalFeeSat` as a `btcutil.Amount`. Called by `Board` and `SendVTXO` so the client's implicit fee matches the server's `validateOperatorFee` under the seal-time fee schedule. Returns zero when `serverConn` is nil (degraded mode).
- `autoRefreshFeeQuoter` — Returns a `vtxo.RefreshFeeQuoter` closure wired into every VTXO actor for auto-refresh fee estimation. Advisory only under the #270 seal-time handshake: the closure's return value is carried on `RefreshVTXORequest.OperatorFee` for observability but is not written to the intent; the server's seal-time compute is authoritative. Falls back to `terms.MinOperatorFee` when the operator is unreachable.
- `SweepBoardingUTXOs` — RPC handler that sweeps CSV-mature boarding UTXOs back to the wallet. Resolves candidates (explicit outpoints or all confirmed/failed/expired intents), estimates the fee, builds and signs an aggregate boarding sweep tx via `buildBoardingSweepTx`, persists the record, broadcasts it, and wakes the `boardingSweepWatcher` to register spend watches. Returns a preview (no broadcast) when `broadcast=false` or no mature outputs exist.
- `ListBoardingSweeps` — RPC handler that returns paginated persisted aggregate boarding sweeps, with optional status filter and cursor-based pagination via a numeric offset token.
- `boardingSweepWatcher` — Daemon-owned background watcher that resumes pending boarding sweeps on startup, rate-limited rebroadcasts published sweeps, and registers chain spend notifications via `chainsource.RegisterSpend` per input. Marks sweep inputs spent when confirmed. Started by `startBoardingSweepWatcher` on wallet unlock; idempotent so the unlock path is safe to call multiple times.
- `boardingSweepTx` / `buildBoardingSweepTx` — Constructs and signs one aggregate timeout-path sweep transaction. Iterates the weight estimate up to three times until `SerializeSize` converges so the returned `fee` and `txid` are accurate. Validates fee-percent guard (`defaultBoardingSweepMaxFeePercent = 25%`) and input cap (`defaultBoardingSweepMaxInputs = 100`).
- `OORConfig` / `OORLimitsConfig` — Configuration block for the OOR actor's incoming receive safety caps (`MaxCheckpoints`, `MaxVTXOMatches`, `MaxMailboxItems`, `MaxMailboxScriptBytes`). `Config.OORReceiveLimits()` normalizes these into `oor.ReceiveLimits` for wiring into `ClientActorCfg.Limits`.
- `deriveIdentityKeyEarly` — Derives the client's secp256k1 identity key from LND or lwwallet before mailbox transport starts. Propagates wallet-specific errors on failure.
- `signMailboxAuth` — Produces Schnorr auth signature. LND path uses tagged Schnorr signing RPC (`withSchnorrTag`); lwwallet path signs locally via `serverconn.SignMailboxAuth`.
- `fetchOperatorPubKeyDirect` — Fetches operator pubkey via direct gRPC `GetInfo` call before the mailbox runtime starts.
- `reserveCustomInputs` (on `RPCServer`) — Atomically claims every custom OOR outpoint for the duration of a `SendOOR` call. Rejects if any outpoint is already reserved. Returns a release function (typically deferred) that frees all claimed outpoints. Prevents two concurrent `SendOOR` callers from double-signing the same vHTLC claim or other non-wallet-managed input.
- `initUnrollSubsystem` — Wires the unilateral-exit runtime during `startWalletDependentActors` (step 12, before `initOORActor`). Order of operations: build a `{txconfirm.Wallet, unroll.SweepWallet}` adapter for the active backend (`lndUnrollWallet` / `lwUnrollWallet` / `btcwUnrollWallet`), register the shared `txconfirm.TxBroadcasterActor` under service key `"txconfirm"`, construct the `unroll.UnrollRegistryActor` with the `db.UnilateralExitPersistenceStore`, `LocalProofAssembler`, shared `txConfirmRef`, and unroll wallet, then call `RestoreNonTerminal(ctx)` to re-ingest jobs persisted across restart. Finally builds a `MapInputRef` that translates `vtxo.ExpiringNotification` → `unroll.EnsureUnrollRequest{Trigger: TriggerCriticalExpiry}` and hands it to `lazyChainResolver.Set`, which resolves the forwarding ref that every VTXO actor is already holding.
- `unrollMaxFeeRate` — Reads `cfg.Unroll.MaxFeeRateSatPerVByte` if positive, else returns zero so each downstream subsystem falls back to its own default. Shared between `txconfirm` and the unroll registry to avoid drift.
- `lndUnrollWallet` / `lwUnrollWallet` / `btcwUnrollWallet` — Backend-specific adapters that satisfy both `txconfirm.Wallet` (ListUnspent / NewWalletPkScript / FinalizePsbt / LeaseOutput / ReleaseOutput) and `unroll.SweepWallet`. LND forwards to the `BoardingBackend`; the lwwallet and btcwallet paths reach into `BtcWallet` directly, reinterpreting `wallet.LockID` as `wtxmgr.LockID` ([32]byte direct cast) so leases round-trip across restart.
- `Unroll` — RPC handler for manual unilateral exit. Parses the outpoint, short-circuits with `Created=false` if the VTXO is already in `VTXOStatusUnilateralExit`, else Asks the VTXO manager an `actormsg.ForceUnrollRequest{Reason: "manual RPC request"}` so the VTXO actor transitions cleanly through `UnilateralExitState`. The registry job is created asynchronously off the manager's outbox via the chain resolver seam; the response returns `unroll.ActorIDForTarget(outpoint)` so callers can poll `GetUnrollStatus`. The Ask/Await calls use a `manualUnrollAdmissionTimeout`-bounded context derived via `context.WithoutCancel(ctx)` so a CLI disconnect does not cancel the daemon-local unilateral-exit handoff.
- `ancestryFromRPC(paths []*arkrpc.AncestryPath) ([]vtxo.Ancestry, error)` — Converts indexer-returned `AncestryPath` protos into the typed `vtxo.Ancestry` slice used by incoming metadata pipelines. Rejects empty slices (version-skew producers that still send the retired tree_path/tree_depth scalars fail closed) and slices exceeding `maxAncestryPaths = 64` (defense-in-depth against misbehaving indexers).
- `GetUnrollStatus` — Read-through RPC handler. Prefers the live `unroll.UnrollRegistryActor` via `queryUnrollRegistry` (which projects the live `JobState` onto the proto enum), and falls back to `db.UnilateralExitPersistenceStore.GetJob` for terminal/evicted jobs. Returns `Found=false` (not an error) when neither layer has a record, so CLI callers can distinguish "no job" from "lookup failed".
- `queryUnrollRegistry` — Asks the registry actor an `unroll.GetStatusRequest`; projects the `GetStatusResp` onto the proto status, preferring `State.Phase` / `State.SweepTxid` / `State.FailReason` for `Active` jobs and falling back to the response's terminal snapshot fields otherwise.
- `unrollPhaseToProto` / `unrollJobStatusToProto` — Dual mappers from the live `unroll.Phase` enum and the persisted `db.UnilateralExitJobStatus` enum into the same proto `UnrollJobStatus`. `PhaseSweepBroadcast` and `PhaseSweepConfirmation` both project to `UNROLL_JOB_STATUS_SWEEPING` so the RPC surface collapses the two internal sweep sub-phases into one client-visible state.
- `NewWalletAddress` / `ListWalletUnspent` (in `wallet_testhooks.go`) — Backend-agnostic harness helpers that return a fresh backing-wallet P2TR address and the current confirmed UTXO set, respectively, regardless of whether the active backend is LND, lwwallet, or btcwallet. Used by system tests that need to fund the daemon's wallet without knowing the backend type.

## Relationships

- **Depends on**: `baselib/actor` (ActorSystem), `btcwbackend`, `chainbackends`,
  `chainsource`, `lib/actormsg`, `db`, `ledger` (accounting actor), `round`,
  `txconfirm`, `unroll`, `vtxo`, `wallet`, `walletcore`, `oor`, `serverconn`,
  `indexer`, `arkrpc`, `lndbackend`, `fraud` (recipient fraud watcher),
  `harness` (bitcoind package submitter wiring in `cmd/darepod`).
- **Depended on by**: `cmd/darepod` (main entry point).

## Invariants

- Server owns ActorSystem lifetime; `Server.run` registers a deferred `actorSystem.Shutdown()` before the deferred `db.Close()` so all actor DB transactions drain before the connection pool is torn down. Without this ordering, in-flight actor lease loops produce "sql: database is closed" warnings at the tail of every itest.
- Server owns ActorSystem lifetime; shutdown stops all subsystems.
- Wallet transitions None → Locked → Ready (or direct to Ready if seed provided).
- Three wallet modes: LND-backed, lightweight (`lwwallet`), or neutrino-backed (`btcwallet` via `btcwbackend`).
- Mailbox IDs are derived from identity pubkeys (via `serverconn.PubKeyMailboxID`), not config strings. The operator's remote mailbox ID is fetched via direct gRPC before the mailbox runtime starts.
- Auth headers (Schnorr signature) are injected into all outbound envelopes including response envelopes in `handleInboundRPC`.
- TLS client cert generation is skipped in insecure mode.
- Per-subsystem logging: configurable log writer, no global mutable loggers. Each subsystem receives its own logger instance.
- All sub-stores share the single `s.clk` clock instance assigned at `NewServer`. New code must not call `clock.NewDefaultClock()` inside `init*` methods — use `s.clk` so tests can inject deterministic time.
- Board RPC is non-blocking: delegates to wallet actor and returns immediately.
- `SendVTXO` enforces a hard recipient cap (`maxRecipients = 256`, see TODO #241), rejects per-recipient amounts outside `(0, MaxSatoshi]`, and uses overflow-safe accumulation when summing recipient amounts. Wallet-side validation (`handleSendVTXOs`) repeats these checks as a defense-in-depth boundary.
- `SendOOR` with custom inputs uses `reserveCustomInputs` to serialize concurrent calls on the same outpoints. Custom inputs are locked for the RPC lifetime; the lock is released via deferred release on both success and failure paths. Standard wallet-managed VTXOs are separately locked via the VTXO manager's reservation flow.
- `BuildCustomTransferInputs` validates that (a) the caller-supplied policy template compiles to the provided pkScript (via `PolicyTemplate.MatchesPkScript`), and (b) the spend path's control block commits to the same pkScript (via `SpendPath.VerifyBindsToPkScript`). Together these prevent a caller from obtaining signatures for an unrelated tapscript by claiming a different output's policy template.
- ListRounds splits pending (in-memory from actor) and persisted (SQL with cursor pagination) rounds.
- Server holds a `roundStore` reference for direct SQL queries from the RPC layer.
- Actor startup order: VTXO manager starts before round actor and OOR actor, so the manager ref is available for both. The round actor ref in the VTXO manager is lazy (service-key-based, resolved at Tell time).
- `mapRoundVTXOManagerMsg` bridges `round.VTXOManagerMsg` → `vtxo.ManagerMsg` via `MapInputRef`. Compile-time assertions enforce that all `round.VTXOManagerMsg` implementors satisfy `vtxo.ManagerMsg`.
- OOR receive-key is derived once at startup via `EnsureDefaultOORReceiveScript` and persisted for restart-safe re-registration. The `DurableUnaryBuilder` is wired through `serverconn.ConnectorConfig` so all indexer queries flow through the durable transport path.
- The OOR artifact store backs three different round/vtxo abstractions via the `ownedScript*Adapter` types: `round.OwnedScriptChecker`, `round.OwnedScriptRegistrar`, and `vtxo.OwnedScriptLookup`. There is one logical "owned receive scripts" table; all ownership questions resolve through it.
- The incoming VTXO handler actor (`vtxo.IncomingVTXOHandler`) is registered with the actor system under `vtxo.IncomingVTXOServiceKey()` during `initOORActor`. The mailbox route `MethodIncomingVTXO` decodes `arkrpc.IncomingVTXOEvent` push notifications and dispatches them to this actor for materialization.
- Every producer actor (`wallet.NewArk`, `round.RoundClientConfig`, `vtxo.ManagerConfig`, `oor.ClientActorCfg`) is wired with `fn.Some(ledger.NewSink(s.actorSystem))` during its `init*Actor` call so emission sites can fire-and-forget ledger messages via the service-key-backed router. `wallet.NewArk` takes the sink as a required constructor argument (not a setter) so every call site must make an explicit emission choice; test harnesses that don't register a ledger actor pass `fn.None[ledger.Sink]()`.
- `EstimateFee` and `GetFeeHistory` both route upstream errors through `proxyUpstreamError` to preserve gRPC codes and strip upstream message detail before it crosses the daemon→client boundary. `GetFeeHistory` further validates request bounds locally (limit positive, offset within `int32` range) before hitting the DB so malformed RPC input cannot trigger a SQL error path.
- In btcwallet mode, neutrino is pre-started before seed availability so P2P sync proceeds in parallel. The `neutrinoSvc` field uses `fn.Option` and is reused by `startBtcwallet` via `NewWithNeutrino`.
- The neutrino sync-wait goroutine polls indefinitely (no timeout) to avoid leaving the wallet permanently unready. Progress is logged every 30 seconds.
- `ensureRoundExists` in `db/vtxo_store.go` uses check-then-insert (not upsert) because `InsertRound`'s `ON CONFLICT DO UPDATE` would overwrite richer round state.
- The unroll subsystem is wired strictly AFTER the VTXO manager but BEFORE the OOR actor. The VTXO manager is created with a `vtxo.LazyChainResolver` placeholder (`s.lazyChainResolver = vtxo.NewLazyChainResolver()`) so VTXO actors spawned during manager construction can hold a stable ref to the resolver; `initUnrollSubsystem` later calls `lazyChainResolver.Set(...)` to point every existing VTXO actor at the live unroll registry without a restart. Any code that also needs this seam must be careful to run AFTER `initUnrollSubsystem` or it will see an unset target.
- `initUnrollSubsystem` creates its own `dbStore` + `vtxoStore` from `s.db` to decouple the unroll store lifecycle from the VTXO manager's; the persisted `s.ueStore` is reused by the `GetUnrollStatus` RPC fallback path so terminal jobs remain queryable after the registry actor evicts them.
- `Server.run` registers a deferred `s.unrollRegistry.Stop()` during startup so the registry's durable persist writer drains before the actor system tears down; without this the final checkpoint of in-flight jobs could race with shutdown.
- `registerOOREventRoutes` checks for `*oorpb.SubmitRejectedError` before a generic error check on the submit-package response. A typed server-side rejection (e.g. `OOR_REJECT_LINEAGE_TOO_LARGE`) is converted to an `oor.OutboxErrorEvent{Retryable: false}` rather than surfaced as an Adapt error, preventing the serverconn ingress dispatcher from stalling the cursor on a sticky rejection that would replay indefinitely.
- `Unroll` and `GetUnrollStatus` guard on `s.vtxoMgrRef.IsSome()` / `s.unrollRegistryRef.IsSome()` and return `codes.Unavailable` (not `Internal`) when the subsystem is not yet initialized so clients can retry rather than treat this as a permanent failure.
- `SweepBoardingUTXOs` always persists the sweep record before broadcasting; on broadcast failure the record is marked failed so the watcher does not attempt rebroadcast. The spend watcher is refreshed via `getBoardingSweepWatcher().Refresh` (using `context.WithoutCancel`) immediately after a successful broadcast so spend notifications start without waiting for the next poll interval.
- `boardingSweepWatcher` uses two cancellation scopes: the per-watcher `w.ctx` for spend registration and the per-refresh `ctx` for rebroadcast RPCs. Spend registration context must be the watcher lifetime so a CLI disconnect does not cancel live spend notifications.
- `OORConfig.OOR.Limits` fields are validated during `Config.Validate()`; `MaxMailboxScriptBytes` must be at least `minOORMailboxScriptBytes = 34` (P2TR script length) to avoid silently rejecting all scripts.
- `quoteOperatorFee` returns `codes.Unavailable` (not `codes.Internal`) when `serverConn` is nil so callers that can fall back to `MinOperatorFee` can distinguish transient from permanent failures.

## Deep Docs

- [docs/daemon_cli_guide.md](../docs/daemon_cli_guide.md) — Installation, configuration, CLI reference.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
