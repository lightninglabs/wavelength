# sdk/ark

## Purpose

Consumer-facing Go SDK facade over a `darepod` runtime. Supports remote
daemon connections, embedded in-process daemon hosting, and wrapping an
already-running in-process daemon RPC server behind a private bufconn
transport, without duplicating Ark runtime behavior.

`EmbeddedConfig`/`StartEmbedded` are split by build tag: `embedded.go`
(`//go:build !js || !wasm`) hosts the real native darepod-backed
implementation, while `embedded_wasm.go` (`//go:build js && wasm`) hosts a
browser stub with an untyped `DaemonConfig any` and a `StartEmbedded` that
always returns an error, so browser builds of dependents (e.g. `sdk/swaps`)
don't have to import the native daemon. `transport.go` holds the
build-tag-independent bufconn dial-readiness helper (`waitForReady`,
`defaultBufConnSize`) shared by both.

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
  models for daemon status and wallet bootstrap flows. `ServerInfo` no
  longer carries `ForfeitScript`/`SweepKey`/`SweepDelay` (dropped); it now
  has `MinVTXOAmountSat` and `MaxUserBalance`, and the old
  `MaxBoardingAmount` field was renamed to `MaxVTXOAmount` (it caps
  boarding requests, round outputs, and OOR recipient outputs alike, not
  just boarding).
- `VTXOExpiryInfo` — SDK-owned typed view of a VTXO's expiry
  classification (`Status daemonrpc.VTXOExpiryStatus`, `CurrentHeight`,
  `BatchExpiry`, `BlocksRemaining`, `RefreshThresholdBlocks`,
  `CriticalThresholdBlocks`, `RelativeExpiry`, `MaxTreeDepth`,
  `ChainDepth`). Embedded as `VTXOInfo.ExpiryInfo` (nil when the daemon
  didn't attach it) and returned standalone by `GetVTXOExpiryInfo`.
- `VTXOInfo` — Typed VTXO view (Outpoint, AmountSat, Status, BatchExpiry,
  RoundID, CreatedHeight, ExpiryInfo, etc.) returned by `ListLiveVTXOs` /
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
  `*daemonrpc.ListOORSessionsResponse`; `ListLocalOORSessions` is preferred
  for new callers.
- `GetVTXOExpiryInfo(ctx, *daemonrpc.GetVTXOExpiryInfoRequest)
  (*daemonrpc.GetVTXOExpiryInfoResponse, error)` — Passthrough asking the
  daemon to classify a VTXO's expiry posture via the authoritative
  wallet/VTXO expiry policy; nil request becomes an empty request.
- `RefreshCustomVTXOs(ctx, *daemonrpc.RefreshCustomVTXOsRequest)
  (*daemonrpc.RefreshCustomVTXOsResponse, error)` — Passthrough queuing
  caller-supplied custom-policy VTXOs for refresh in the next round; nil
  request becomes an empty request and surfaces the daemon's validation
  error.
- `SignVTXOForfeit(ctx, *daemonrpc.SignVTXOForfeitRequest)
  (*daemonrpc.SignVTXOForfeitResponse, error)` — Passthrough asking the
  daemon wallet to sign one exact connector-bound forfeit transaction
  input.
- `SendOOR` recipients are a slice: requests take `Recipients
  []*daemonrpc.Output` (not a single `Recipient`), and
  `SendOORResponse.RecipientOutpoints` is a `[]string` (not a singular
  `RecipientOutpoint`). `SendOORWithPolicyAndKeyDetails` and
  `SendOORWithCustomInputs` send one-element slices and read index `[0]`
  of the outpoints slice.

## Relationships

- **Depends on**: `daemonrpc`, `darepod` (embedded mode only), gRPC,
  `google.golang.org/grpc/test/bufconn` (in-process transport).
- **Depended on by**: `sdk/swaps` (type aliases, receive-auth RPCs, OOR
  helpers), Go hosts that want remote, embedded, or in-process Ark client
  access.

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
- `ServerInfo` is nil until the daemon reaches operator-bootstrap; beyond
  the initial bootstrap fetch it can also be refreshed by daemon paths that
  fetch the live operator key before building new policy scripts.
- Pre-1.0, some methods intentionally return `daemonrpc` protobuf types
  directly. Those passthrough APIs are not yet treated as stable SDK-owned
  models.
- Receive-auth signing and ECDH are always delegated to the daemon; the SDK
  never holds raw private key material for receive-auth operations.
