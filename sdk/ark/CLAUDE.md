# sdk/ark

## Purpose

Consumer-facing Go SDK facade over a `darepod` runtime. Supports remote
daemon connections, embedded in-process daemon hosting, and wrapping an
already-running in-process daemon RPC server behind a private bufconn
transport, without duplicating Ark runtime behavior.

## Key Types

- `Client` — Concurrency-safe SDK handle around a `daemonrpc` client. Owns
  transport shutdown and, in embedded mode, exposes `Wait()` for the daemon
  run result. Constructed via `DialRemote`, `StartEmbedded`,
  `WrapDaemonClient`, or `WrapDaemonServer`.
- `RemoteConfig` — Remote daemon dialing config. Secure by default: callers
  must provide transport credentials or explicitly opt into insecure
  transport for local development.
- `EmbeddedConfig` — In-process daemon hosting config. Currently passes
  through a cloned `*darepod.Config`; the SDK hides transport and lifecycle
  management, not the full daemon config surface.
- `InProcessConfig` — Config for `WrapDaemonServer`. Wraps an
  already-running `daemonrpc.DaemonServiceServer` behind a private
  bufconn-backed gRPC server. Holds the `DaemonServer`, optional
  `BufferSize`, `DialOptions`, and `ServerOptions`. The returned `Client`
  owns only the private transport, not the supplied daemon runtime.
- `WrapDaemonServer` — Constructor that creates a `Client` facade
  over an in-process daemon RPC implementation without dialing the daemon's
  public network listener. Used for tight in-process integration where the
  host already owns the daemon runtime.
- `WrapDaemonClient` — Constructor that creates a `Client` from an
  already-connected `daemonrpc.DaemonServiceClient` and a caller-supplied
  `closeFn`.
- `Info` / `ServerInfo` / `Seed` / `WalletInitResult` — SDK-owned typed
  models for daemon status and wallet bootstrap flows.
- `VTXOInfo` — Typed VTXO view (Outpoint, AmountSat, Status, BatchExpiry,
  RoundID, CreatedHeight, etc.) returned by `ListLiveVTXOs` /
  `ListSpentVTXOs`.
- `ReceiveInfo` — Typed receive destination (PkScript, PubKeyXOnly) returned
  by `NewReceiveScript` / `AllocateReceiveScript`.
- `IndexedOORSessionInfo` — Indexed OOR session view (ArkPSBT,
  CheckpointPSBTs) returned by `GetIndexedOORSession` lookups.
- `CustomOORInput` — Caller-specified OOR input carrying a policy template,
  spend path, and UTXO info for `SendOORWithCustomInputs`.
- Policy/OOR helpers such as `SendOORWithPolicy`, `SendOORWithCustomInputs`,
  typed indexed VTXO lookups, and typed receive-script decoding belong here
  so higher-level packages do not rebuild daemonrpc adapters.
- Receive-auth helpers: `ReceiveAuthKey`, `SignReceiveAuthMessage`,
  `SignReceiveAuthMessageCompact`, `ReceiveAuthECDH` — delegate payment-scoped
  signing and Sphinx ECDH operations to the daemon wallet without exposing the
  raw private key to the SDK caller. Used by `sdk/swaps` for receive invoice
  signing and onion decoding.
- `GetOORSession(ctx, sessionID) (*daemonrpc.OORSessionInfo, error)` —
  passthrough lookup of the daemon's local durable status for one OOR
  session. There is no SDK-owned local-session-listing API; the previous
  `ListLocalOORSessions`/`ListPendingOORSessions`/`OORSessionDirection`
  surface was removed once caller-intent keys replaced local session
  enumeration as the retry primitive.
- `OORSendResult`, `SendOORWithPolicyDetails`,
  `SendOORWithPolicyAndKeyDetails` — accepted-OOR metadata (session id,
  resolved recipient outpoint) for policy-backed sends. `SendOORWithPolicy`
  and `SendOORWithPolicyAndKey` are session-id-only convenience wrappers
  over the same calls.
- `PreparedOOR`, `PreparedOORCustomInput`, `TaprootScriptSignature` —
  deterministic custom-input OOR package and external-signature types
  produced by `PrepareOORWithCustomInputs` and consumed by
  `SignOORCustomInput`, letting multi-party custom-input flows stage
  signatures before submission.
- `VTXOExpiryInfo` — Typed VTXO expiry classification (thresholds, chain
  depth, tree depth) returned by `GetVTXOExpiryInfo`.
- VHTLC recovery passthroughs `ArmVHTLCRecovery`, `EscalateVHTLCRecovery`,
  `CancelVHTLCRecovery`, `GetVHTLCRecoveryStatus` — persist, escalate,
  cancel, and poll dormant vHTLC unroll recovery jobs on behalf of
  higher-level swap FSMs.

## Relationships

- **Depends on**: `daemonrpc`, `darepod` (embedded mode only), gRPC,
  `google.golang.org/grpc/test/bufconn` (in-process transport).
- **Depended on by**: `sdk/swaps` (type aliases, receive-auth RPCs, OOR
  helpers), `swapclientserver` (`WrapDaemonServer` in-process wiring,
  `ServerInfo`), Go hosts that want remote, embedded, or in-process Ark
  client access.

## Invariants

- `Client` is safe for concurrent use.
- `darepod` remains the canonical Ark runtime; `sdk/ark` must not
  reimplement wallet, round, OOR, or persistence behavior.
- Embedded startup must not mutate the caller's daemon config.
- Embedded startup waits until the in-process daemon is accepting RPCs
  before returning.
- Embedded `Wait()` returns a blocking channel that surfaces the daemon's
  terminal run error.
- `Close()` is idempotent and bounds embedded shutdown wait time.
- `WrapDaemonServer` owns only the private bufconn transport and gRPC server;
  it does not own the caller's `DaemonServer` runtime. `Close()` tears down
  only the private transport.
- `ServerInfo` is a bootstrap-time operator-terms snapshot; refresh after
  reconnect is not wired through yet.
- Pre-1.0, some methods intentionally return `daemonrpc` protobuf types
  directly. Those passthrough APIs are not yet treated as stable SDK-owned
  models.
- Receive-auth signing and ECDH are always delegated to the daemon; the SDK
  never holds raw private key material for receive-auth operations.
- `StartEmbedded` is unavailable in `js/wasm` builds (`embedded_wasm.go`) and
  always returns an error there; `EmbeddedConfig.DaemonConfig` is typed `any`
  in that build so wasm-only callers can depend on `sdk/ark` without
  importing the native `darepod` package.
