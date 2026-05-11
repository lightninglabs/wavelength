# sdk/ark

## Purpose

Consumer-facing Go SDK facade over a `darepod` runtime. Supports both
remote daemon connections and embedded in-process daemon hosting
without duplicating Ark runtime behavior.

## Key Types

- `Client` — Concurrency-safe SDK handle around a `daemonrpc`
  client. Owns transport shutdown and, in embedded mode, exposes
  `Wait()` for the daemon run result.
- `RemoteConfig` — Remote daemon dialing config. Secure by default:
  callers must provide transport credentials or explicitly opt into
  insecure transport for local development.
- `EmbeddedConfig` — In-process daemon hosting config. Currently
  passes through a cloned `*darepod.Config`; the SDK hides transport
  and lifecycle management, not the full daemon config surface.
- `InProcessConfig` — Config for wrapping an already-running daemon RPC
  server implementation in the same process. Holds `DaemonServer
  daemonrpc.DaemonServiceServer`, optional `BufferSize`, `DialOptions`,
  and `ServerOptions`. Does not own the daemon runtime — `Close` only
  tears down the private bufconn transport.
- `WrapDaemonServer(ctx, InProcessConfig) (*Client, error)` — Creates an
  Ark SDK facade over an in-process gRPC server via a private bufconn
  listener. Useful when the swap client server needs an Ark client
  without a separate network listener (e.g. for invoice generation inside
  `swapclientserver`).
- `Info` / `ServerInfo` / `Seed` / `WalletInitResult` — SDK-owned
  typed models for daemon status and wallet bootstrap flows.
- `VTXOInfo` — Typed VTXO view (Outpoint, AmountSat, Status,
  BatchExpiry, RoundID, CreatedHeight, etc.) returned by
  `ListLiveVTXOs` / `ListSpentVTXOs`.
- `ReceiveInfo` — Typed receive destination (PkScript,
  PubKeyXOnly) returned by `NewReceiveScript` /
  `AllocateReceiveScript`.
- `IndexedOORSessionInfo` — Indexed OOR session view (ArkPSBT,
  CheckpointPSBTs) returned by `GetIndexedOORSession` lookups.
- `CustomOORInput` — Caller-specified OOR input carrying a policy
  template, spend path, and UTXO info for `SendOORWithCustomInputs`.
- Policy/OOR helpers such as `SendOORWithPolicy`,
  `SendOORWithCustomInputs`, typed indexed VTXO lookups, and typed
  receive-script decoding belong here so higher-level packages do not
  rebuild daemonrpc adapters.
- `OORSessionDirection` — Enum (`OORSessionDirectionAll`,
  `OORSessionDirectionOutgoing`, `OORSessionDirectionIncoming`) for
  filtering local OOR session listings.
- `ListOORSessionsRequest` — Filter struct: `PendingOnly bool`,
  `Direction OORSessionDirection`.
- `OORSessionInfo` — Typed view of one locally persisted OOR session:
  `SessionID`, `Direction`, `Phase`, `Pending`, `RetryAfter`,
  `RetryReason`, `InputOutpoints`, `InputAmountSat`, `RecipientCount`.
- `ListLocalOORSessions(ctx, ListOORSessionsRequest) ([]OORSessionInfo,
  error)` — Typed wrapper converting proto response to SDK types.
- `ListPendingOORSessions(ctx) ([]OORSessionInfo, error)` — Convenience
  wrapper calling `ListLocalOORSessions` with `PendingOnly: true`.
- `ListOORSessions` — Lower-level passthrough returning the raw
  `*daemonrpc.ListOORSessionsResponse`; `ListLocalOORSessions` is
  preferred for new callers.

## Relationships

- **Depends on**: `daemonrpc`, `darepod`, gRPC, `bufconn` (for
  `WrapDaemonServer` in-process transport).
- **Depended on by**: `sdk/swaps` (type aliases and daemon connection),
  `swapclientserver` (in-process Ark client for invoice generation),
  Go hosts that want remote or embedded Ark client access.

## Invariants

- `Client` is safe for concurrent use.
- `darepod` remains the canonical Ark runtime; `sdk/ark` must not
  reimplement wallet, round, OOR, or persistence behavior.
- `WrapDaemonServer` does not own the supplied daemon runtime; `Close` only
  tears down the private bufconn listener and gRPC server.
- Embedded startup must not mutate the caller's daemon config.
- Embedded startup waits until the in-process daemon is accepting RPCs
  before returning.
- Embedded `Wait()` returns a blocking channel that surfaces the
  daemon's terminal run error.
- `Close()` is idempotent and bounds embedded shutdown wait time.
- `ServerInfo` is a bootstrap-time operator-terms snapshot; refresh
  after reconnect is not wired through yet.
- Pre-1.0, some methods intentionally return `daemonrpc` protobuf
  types directly. Those passthrough APIs are not yet treated as
  stable SDK-owned models.
