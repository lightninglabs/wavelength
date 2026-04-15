# darepod

## Purpose

Top-level daemon orchestrator that wires wallet backend, mailbox transport,
chain backend, database, and all domain actors into a running system with a
gRPC API.

## Key Types

- `Server` — Main daemon owning wallet, DB, chainsource actor, gRPC server, and ActorSystem. Caches `localMailboxID` (pubkey-derived), `authSigHex` (Schnorr auth) and a single `clk` (`clock.Clock`) that all sub-stores share for deterministic time injection.
- `RPCServer` — Implements the gRPC `DaemonService` API (Board, ListRounds, WatchRounds, NewOORReceiveScript, SendVTXO, etc.). Includes test hooks for mailbox edge factory and round registration. Holds an in-memory `customInputLocks` map (guarded by `customInputLocksMu`) that reserves custom OOR input outpoints for the duration of a `SendOOR` call to prevent concurrent callers from double-signing the same custom input.
- `Config` — Daemon configuration (data dir, network, RPC host, wallet type, etc.). Includes `MailboxEdgeFactory` hook for test harness transport interception.
- `TriggerRoundRegistration` — Test-hook method that injects a round registration event into the round actor (in `server_round_testhook.go`).
- `GetStoredVTXO` — Harness-only accessor that returns a persisted `vtxo.Descriptor` for a given outpoint directly from the daemon's VTXO store. Lets integration tests inspect partial unroll state without reaching into internal fields.
- `WalletState` — Enum (None/Locked/Ready) for wallet lifecycle.
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
- `initLedgerActor` — Constructs `ledger.LedgerActor` with both `db.NewLedgerStoreDB` (double-entry ledger) and `db.NewUTXOAuditStoreDB` (UTXO audit log) as stores, starts it, and registers it with the actor system under `ledger.ServiceKeyName`. Called in `run` after the DB and delivery store are ready but before wallet unlock, since the actor does not depend on wallet state.
- `deriveIdentityKeyEarly` — Derives the client's secp256k1 identity key from LND or lwwallet before mailbox transport starts. Propagates wallet-specific errors on failure.
- `signMailboxAuth` — Produces Schnorr auth signature. LND path uses tagged Schnorr signing RPC (`withSchnorrTag`); lwwallet path signs locally via `serverconn.SignMailboxAuth`.
- `fetchOperatorPubKeyDirect` — Fetches operator pubkey via direct gRPC `GetInfo` call before the mailbox runtime starts.
- `reserveCustomInputs` (on `RPCServer`) — Atomically claims every custom OOR outpoint for the duration of a `SendOOR` call. Rejects if any outpoint is already reserved. Returns a release function (typically deferred) that frees all claimed outpoints. Prevents two concurrent `SendOOR` callers from double-signing the same vHTLC claim or other non-wallet-managed input.

## Relationships

- **Depends on**: `baselib/actor` (ActorSystem), `btcwbackend`, `chainbackends`, `chainsource`, `lib/actormsg`, `db`, `ledger` (accounting actor), `round`, `vtxo`, `wallet`, `walletcore`, `oor`, `serverconn`, `indexer`, `arkrpc`.
- **Depended on by**: `cmd/darepod` (main entry point).

## Invariants

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
- In btcwallet mode, neutrino is pre-started before seed availability so P2P sync proceeds in parallel. The `neutrinoSvc` field uses `fn.Option` and is reused by `startBtcwallet` via `NewWithNeutrino`.
- The neutrino sync-wait goroutine polls indefinitely (no timeout) to avoid leaving the wallet permanently unready. Progress is logged every 30 seconds.
- `ensureRoundExists` in `db/vtxo_store.go` uses check-then-insert (not upsert) because `InsertRound`'s `ON CONFLICT DO UPDATE` would overwrite richer round state.

## Deep Docs

- [docs/daemon_cli_guide.md](../docs/daemon_cli_guide.md) — Installation, configuration, CLI reference.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
