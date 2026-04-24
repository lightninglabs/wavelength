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
- `Info` / `ServerInfo` / `Seed` / `WalletInitResult` — SDK-owned
  typed models for daemon status and wallet bootstrap flows.
- `VTXOInfo` — Typed VTXO view (Outpoint, AmountSat, Status,
  BatchExpiry, RoundID, CreatedHeight, etc.) returned by
  `ListLiveVTXOs` / `ListSpentVTXOs`.
- `OORReceiveInfo` — Typed OOR receive destination (PkScript,
  PubKeyXOnly) returned by `NewOORReceiveScript` /
  `AllocateOORReceiveScript`.
- `IndexedOORSessionInfo` — Indexed OOR session view (ArkPSBT,
  CheckpointPSBTs) returned by `GetIndexedOORSession` lookups.
- `CustomOORInput` — Caller-specified OOR input carrying a policy
  template, spend path, and UTXO info for `SendOORWithCustomInputs`.
- Policy/OOR helpers such as `SendOORWithPolicy`,
  `SendOORWithCustomInputs`, typed indexed VTXO lookups, and typed
  receive-script decoding belong here so higher-level packages do not
  rebuild daemonrpc adapters.

## Relationships

- **Depends on**: `daemonrpc`, `darepod`, gRPC.
- **Depended on by**: future `sdk/swaps`, Go hosts that want remote or
  embedded Ark client access.

## Invariants

- `Client` is safe for concurrent use.
- `darepod` remains the canonical Ark runtime; `sdk/ark` must not
  reimplement wallet, round, OOR, or persistence behavior.
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
