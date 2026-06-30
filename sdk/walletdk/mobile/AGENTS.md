# sdk/walletdk/mobile

## Purpose

gomobile-safe facade over `sdk/walletdk` for Android/iOS native
bindings. Wraps the Go wallet SDK in a flat API that obeys gomobile's
type restrictions (no `context.Context`, no channels, no maps, no
slices except `[]byte`, no unsigned integers at the boundary). Uses a
JSON bytes-in / bytes-out convention for RPC verbs and a pull-based
`Subscription` handle for streaming, avoiding the callback-interface
pattern used by lnd-mobile.

Only compiles under the `mobile`, `walletdkrpc`, and `swapruntime`
build tags — native `go build ./...` sees the empty `stub.go`.

## Key Types

- `Wallet` — Top-level gomobile-safe handle. `Start` is synchronous
  (call off the main thread); returns once gRPC is serving. All verb
  methods are synchronous and return `[]byte` (JSON-encoded response)
  or a `string` error message.
- `Subscription` — Pull-based streaming handle returned by
  `Wallet.Subscribe`. `Next()` blocks until an update arrives (returns
  `[]byte`); `Close()` tears down the stream.
- `Config` — gomobile-safe config struct for `Start`. Wraps network,
  server address, data dir, and other daemon options as plain-type
  fields.
- `PasskeyPRFOutput` — gomobile-safe passkey credential type that
  wraps the raw PRF bytes for `Wallet.OpenWalletFromPasskey`.

## Relationships

- **Depends on**:
  - `sdk/walletdk` (delegates all wallet operations)
- **Depended on by**:
  - `cmd/walletdk-wasm` (imports mobile for the WASM bridge; the
    gomobile binding for native is built separately via `gomobile bind`)
- **Sends** → `sdk/walletdk`: all wallet RPCs via the embedded daemon.
- **Receives** ← platform (Android/iOS/WASM): method calls from the
  host runtime via the generated gomobile or WASM binding layer.

## Invariants

- No `context.Context`, channel, map, or unsigned integer type may
  appear in exported symbols — these types cannot cross the gomobile
  boundary.
- `Start` must be called on a background thread; it blocks until gRPC
  is ready.
- The JSON request/response convention is intentional: the host decodes
  with kotlinx.serialization or Codable, so no protobuf runtime is
  needed on the platform side.
- Build tags (`mobile`, `walletdkrpc`, `swapruntime`) ensure this
  package never contaminates the native build graph.

## Deep Docs

- [sdk/walletdk/CLAUDE.md](../CLAUDE.md) — The underlying Go SDK this
  package wraps.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
