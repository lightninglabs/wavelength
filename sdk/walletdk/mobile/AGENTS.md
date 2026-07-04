# sdk/walletdk/mobile

## Purpose

[gomobile](https://pkg.go.dev/golang.org/x/mobile/cmd/gomobile)-safe flat
facade over `sdk/walletdk` that lets an Android (Kotlin/Java) or iOS
(Swift/ObjC) host drive an embedded `darepod` wallet **in-process** — no
separate daemon binary, no open socket — by reusing the private `bufconn`
gRPC transport `walletdk.Start` already sets up. `gomobile bind` only carries
a narrow set of types across the FFI boundary (signed ints, floats, `string`,
`bool`, `[]byte`, and flat structs/interfaces of those), so this package is a
translation layer that never exposes the richer `walletdk` types directly:
every verb takes/returns JSON `[]byte`, with a pull-based `Subscription`
handle for the one streaming verb and a handful of plain-scalar convenience
functions for hot paths.

## Key Types

- `Start(cfgJSON string) error` / `Stop() error` — package-level singleton
  lifecycle (gomobile exposes free functions, so there is no handle for a
  host to carry across the boundary). A four-state machine
  (`statusStopped`/`statusStarting`/`statusStarted`/`statusStopping`) plus a
  monotonic `gen` counter and a stored `startCancel` close the race where a
  `Stop` races an in-progress `Start`: `Start` cooperatively tears down any
  client it produced instead of publishing it, and a panic in the boot path
  is recovered into the returned error. `Stop` is idempotent and resets the
  singleton so a host can restart after the OS suspends/resumes the app.
- `activeClient() (*walletdk.Client, context.Context, error)` — resolves the
  live client plus the wrapper-owned `callCtx` (created in `Start`, cancelled
  by `Stop`), or an error when not started. Every RPC verb calls this first.
- `mobileConfig` (`config.go`) — flat, JSON-tagged, scalar-only subset of
  `walletdk.Config`: omits `DaemonConfig *darepod.Config` and
  `LogWriter io.Writer` (cannot cross JSON/gomobile), expresses durations as
  integer seconds and amounts as `int64`. `parseConfig` decodes it (empty
  string → `walletdk.DefaultConfig()`), `validate()` rejects negative
  durations/counts and an oversized recovery window before they reach the
  daemon, and `applyMobileConfig` overlays only host-set fields onto the
  defaults (enable-only / non-empty semantics, mirroring `walletdk.Config`).
- RPC verbs (`mobile.go`, `passkey.go`) — JSON `[]byte` in / out, throwing:
  `GetInfo`, `CreateWallet`, `UnlockWallet`, `OpenWalletFromPasskey`,
  `Balance`, `Deposit`, `Receive`, `PrepareSend`, `SendPrepared`, `List`,
  `Exit`, `ExitStatus`, `GetExitPlan`, `SweepWallet`, `Status`. Each decodes
  its request into the matching `walletdk.*Request` DTO (verbatim Go field
  names, PascalCase on the wire — no `json:` tags on those DTOs) and
  marshals the `walletdk.*Result`/response back out via `marshal`.
- `Subscription` (`mobile.go`) — gomobile-safe pull handle over a wallet
  activity stream: `Next() ([]byte, error)` blocks for the next
  `walletdk.Entry` and returns `io.EOF` at a clean end (including a
  self-initiated `Close`/`Stop`, which is reported as EOF rather than a
  `context.Canceled` error); `Close() error` cancels a blocked `Next` and is
  idempotent. Built by `Subscribe(reqJSON []byte) (*Subscription, error)`.
- Scalar convenience functions (`convenience.go`): `ConfirmedBalanceSat`,
  `PendingInboundSat` (`int64`), `WalletReady` (`bool`, GetInfo's wallet
  state shortcut), `IsRunning` (`bool`, `lifecycleActive()` — true across the
  whole boot window, not only once gRPC is serving; does not itself imply
  RPCs will succeed).
- `OpenWalletFromPasskey` (`passkey.go`) — decodes a hex WebAuthn PRF
  assertion output and calls `client.OpenWalletFromPasskey`; the
  PRF→seed derivation lives in Go so this facade and the `walletdk-wasm`
  bridge share one source of truth and the browser/host never handles raw
  seed material.
- `doc.go` — the only file with no build tag; an ordinary `go build ./...`
  sees just this package doc comment (everything else is gated behind
  `mobile`, `walletdkrpc`, and `swapruntime`).
- `gen_bindings.sh` — runs `gomobile bind` with
  `-tags="mobile walletdkrpc swapruntime"` to produce the Android `.aar`
  (`make mobile-android`) and iOS `.xcframework` (`make mobile-ios`).

## Relationships

- **Depends on**: `sdk/walletdk` (`walletdk.Client`, `walletdk.Start`,
  `walletdk.Config`/`DefaultConfig`, every `walletdk.*Request`/`*Result` DTO)
  — the sole non-stdlib dependency; this package owns no wiring of its own.
- **Depended on by**: `cmd/walletdk-wasm` (`js && wasm` build, a
  `syscall/js` adapter that calls this package's free functions directly
  rather than `walletdk.Client`, so the wasm and gomobile bindings can never
  drift from each other), and externally the gomobile-generated `.aar`
  (Android) / `.xcframework` (iOS) artifacts consumed by
  [lightninglabs/damobile](https://github.com/lightninglabs/damobile).
- **Sends**: no actor messages — this package calls `walletdk.Client`
  methods directly (in-process bufconn RPCs under the hood, owned by
  `walletdk`).
- **Receives**: ← host application calls only (JSON `[]byte` verbs, scalar
  convenience calls, and `Subscription.Next` polls). No inbound RPC surface
  of its own.

## Invariants

- Gomobile boundary types only: no `context.Context`, channels, maps,
  `uint*`, `time.Time`, non-`[]byte` slices, or tagged-union structs may
  appear in any exported signature. `mobileConfig` and every request/response
  DTO are flat and JSON-scalar for this reason.
- `Start` **must** be called off the host's main thread — it blocks (up to
  `startTimeout = 90s`) until the embedded daemon's private gRPC transport
  reports ready. The daemon's own lifetime is owned by `walletdk`'s internal
  `runCtx`, not this deadline; `startTimeout` only bounds dialing.
  `//nolint:contextcheck` markers matching `walletdk`'s pattern apply for the
  same reason.
- Singleton-guarded: a second `Start` before a matching `Stop` returns an
  error rather than booting a second daemon. All lifecycle state lives in
  the package-level `state` struct guarded by `state.mu`; every mutation
  bumps `state.gen` so a `Start` that finishes after a racing `Stop` (and a
  possible subsequent `Start`) can detect it no longer owns the boot and
  must not publish its client.
- A panic anywhere in the boot path or in `Subscription.Next` is recovered
  into a returned Go error — it must never cross the gomobile boundary as an
  unrecovered panic, which would kill the host process.
- No host-implemented callback interfaces exist anywhere in this API.
  Streaming is exclusively pull-based (`Subscription.Next` in a host loop);
  unary calls are ordinary throwing verbs. The host owns its own threads.
- JSON (not protobuf) is the wire format for every non-scalar verb, so hosts
  stay free of a protobuf runtime. The verb DTOs carry **no** `json:` tags,
  so wire keys are the literal Go field names (PascalCase), not
  snake_case — the one exception is `mobileConfig`, which is a dedicated
  snake_case-tagged struct.
- `Stop` always fully unwinds: it cancels the wrapper `callCtx` (unblocking
  any in-flight `Subscription.Next`) before tearing down the client, and
  resets the singleton to `statusStopped` so an immediate subsequent `Start`
  cannot race a half-finished shutdown.
- This package must never reach into `walletdk.Client` fields or bypass its
  API — `cmd/walletdk-wasm` intentionally calls through this facade instead
  of `walletdk` directly so the two bindings cannot drift.
- Not yet wired: a host-driven `ProcessNextMsg` mailbox pump cooperative with
  OS suspension (issue #713 open question #2); the package currently relies
  on the embedded daemon's always-on background ingress loop.

## Deep Docs

- [docs/walletdk_mobile.md](../../../docs/walletdk_mobile.md) — Full
  rationale, the hybrid payload contract, lifecycle/streaming semantics,
  config shape, build prerequisites, and the sample-app integration path.
- [sdk/walletdk/CLAUDE.md](../CLAUDE.md) — The wallet SDK this package
  wraps; DTO shapes and RPC method semantics referenced above are defined
  there.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
