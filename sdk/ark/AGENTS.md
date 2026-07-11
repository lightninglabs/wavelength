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
- `RemoteConfig` / `EmbeddedConfig` / `InProcessConfig` — Dial, in-process
  hosting, and bufconn-wrapping configs for the three constructors above.
  `RemoteConfig` is secure by default: callers must supply transport
  credentials or explicitly set `AllowInsecure`. `EmbeddedConfig` has a
  native build (`embedded.go`) and a browser stub build (`embedded_wasm.go`,
  `js && wasm`); shared transport helpers live in `transport.go`.
- `Info` / `ServerInfo` / `Seed` / `WalletInitResult` / `WalletState` —
  SDK-owned models for daemon status, cached operator terms, and wallet
  bootstrap flows. `Info.WalletReady()` checks `WalletState ==
  WalletStateReady`.
- `VTXOInfo` / `VTXOExpiryInfo` — Typed VTXO view and expiry classification
  returned by `ListLiveVTXOs`, `ListSpentVTXOs`, `GetVTXOExpiryInfo`.
- `ReceiveInfo` — Typed receive destination returned by `NewReceiveScript` /
  `AllocateReceiveScript`.
- `CustomOORInput` / `TaprootScriptSignature` / `PreparedOOR` /
  `PreparedOORCustomInput` / `OORSendResult` / `IndexedOORSessionInfo` —
  OOR request/response models used by `SendOORWithCustomInputs`,
  `PrepareOORWithCustomInputs`, `SignOORCustomInput`,
  `SendOORWithPolicyAndKeyDetails`, and `GetIndexedOORSession`.
- VHTLC recovery passthroughs: `ArmVHTLCRecovery`, `EscalateVHTLCRecovery`,
  `CancelVHTLCRecovery`, `GetVHTLCRecoveryStatus` — durable dormant-recovery
  lifecycle for higher-level swap FSMs; return `daemonrpc` types directly.
- Receive-auth helpers: `ReceiveAuthKey`, `SignReceiveAuthMessage`,
  `SignReceiveAuthMessageCompact`, `ReceiveAuthECDH` — delegate
  payment-scoped signing and Sphinx ECDH to the daemon wallet without
  exposing raw key material. Used by `sdk/swaps` for receive invoice signing
  and onion decoding.
- `GetOORSession` — Single-session lookup of the daemon's local durable OOR
  transfer status, returning `*daemonrpc.OORSessionInfo`.
- `Board`, `ListRounds`, `WatchRounds`, `EstimateFee`, `GetFeeHistory` — Round
  and fee passthroughs returning `daemonrpc` request/response types directly.

## Relationships

- **Depends on**: `daemonrpc`, `darepod` (embedded mode only), gRPC,
  `google.golang.org/grpc/test/bufconn` (in-process transport).
- **Depended on by**: `sdk/swaps` (type aliases, receive-auth RPCs, OOR
  helpers), `swapclientserver`, Go hosts that want remote, embedded, or
  in-process Ark client access.

## Invariants

- `Client` is safe for concurrent use.
- `darepod` remains the canonical Ark runtime; `sdk/ark` must not
  reimplement wallet, round, OOR, or persistence behavior.
- Embedded startup must not mutate the caller's daemon config
  (`cloneDaemonConfig` deep-copies reference-typed fields; update it when
  `darepod.Config` gains new reference fields).
- Embedded startup waits until the in-process daemon is accepting RPCs
  before returning.
- Embedded `Wait()` returns a blocking channel that surfaces the daemon's
  terminal run error; remote clients return an already-closed channel.
- `Close()` is idempotent and bounds embedded/in-process shutdown wait time.
- `WrapDaemonServer` owns only the private bufconn transport and gRPC
  server; it does not own the caller's `DaemonServer` runtime. `Close()`
  tears down only the private transport.
- `StartEmbedded` is unavailable in browser builds (`js && wasm`):
  `embedded_wasm.go` supplies a stub `EmbeddedConfig` (untyped
  `DaemonConfig any` so browser callers avoid importing `darepod`) and
  `StartEmbedded` always returns an error there. Native builds keep the
  real `darepod`-backed implementation in `embedded.go`.
- `ServerInfo` is a bootstrap-time operator-terms snapshot; refresh after
  reconnect is not wired through yet.
- Pre-1.0, some methods intentionally return `daemonrpc` protobuf types
  directly. Those passthrough APIs are not yet treated as stable SDK-owned
  models.
- Receive-auth signing and ECDH are always delegated to the daemon; the SDK
  never holds raw private key material for receive-auth operations.

## Deep Docs

- [docs/sdk_layered_architecture.md](../../docs/sdk_layered_architecture.md)
  — Layered SDK architecture, error categorization, daemonrpc versioning
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map
