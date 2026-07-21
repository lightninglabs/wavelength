# wavewalletdk on mobile (gomobile)

`sdk/wavewalletdk/mobile` is a [gomobile](https://pkg.go.dev/golang.org/x/mobile/cmd/gomobile)-safe
facade over the wallet SDK. It lets an Android (Kotlin/Java) or iOS
(Swift/ObjC) host drive an embedded `waved` wallet **in-process** — no
separate daemon binary, no open socket — by reusing the private `bufconn` gRPC
transport that [`wavewalletdk.Start`](../sdk/wavewalletdk/embedded.go) already sets up.

It borrows the *bytes-out* idea from `lightninglabs/falafel` and `lnd/mobile`
**without** the protoc generator or their callback interfaces, because the
`wavewalletdk` facade already owns the wiring and `wavewalletdk.Start` returns once gRPC
is serving (`lnd.Main` blocks forever, so lnd needs a callback — we do not).

## Why a separate package

`gomobile bind` only carries a narrow set of types across the FFI boundary:
signed ints, floats, `string`, `bool`, `[]byte`, and interfaces/structs whose
members are all of those. It cannot carry `context.Context`, channels, maps,
`uint*`, `time.Time`, slices other than `[]byte`, or tagged-union structs — all
of which appear throughout `wavewalletdk`'s public API. So `mobile` is a flat
translation layer; it never exposes the rich `wavewalletdk` types directly.

The package is gated behind three build tags — `mobile`, `wavewalletrpc`, and
`swapruntime` — so it only compiles into a `gomobile bind` output. An ordinary
`go build ./...` sees only the unconstrained `doc.go`.

## The payload contract (hybrid)

| Shape | Convention | Verbs |
|-------|-----------|-------|
| RPC verbs | **JSON `[]byte` in / out** (throwing) | `GetInfo`, `CreateWallet`, `UnlockWallet`, `OpenWalletFromPasskey`, `Balance`, `Deposit`, `Receive`, `PrepareSend`, `SendPrepared`, `List`, `Exit`, `ExitStatus`, `ExitSummary`, `GetExitPlan`, `SweepWallet`, `Status` |
| Streaming | **pull handle** `Subscription{ next() []byte; close() }` | `Subscribe` |
| Hot-path scalars | **plain `int64`/`bool`** | `ConfirmedBalanceSat`, `PendingInboundSat`, `WalletReady`, `IsRunning` |

There are **no host-implemented callback interfaces**. Unary verbs are ordinary
throwing calls; streaming is pull-based. The host owns its own threads — which
is where Kotlin coroutines / Swift `async` live anyway.

JSON (not protobuf) keeps hosts free of a protobuf runtime: decode with
`kotlinx.serialization` on Android or `Codable` on iOS.

Mind the field names. The verb DTOs in `sdk/wavewalletdk/types.go` carry **no**
`json:"…"` tags, so `encoding/json` uses the Go field names verbatim: the wire
keys are PascalCase (`Version`, `ConfirmedSat`, `AmountSat`, `IdentityPubKey`),
not snake_case. Host models must map those exact names (e.g. `@SerialName`
/ `CodingKeys`), or fields silently decode as zero. Two exceptions: the
`Start` config, a dedicated tagged struct (`config.go`) that is snake_case
(`data_dir`, `wallet_esplora_url`, …), and `OpenWalletFromPasskey`, whose
request is a small camelCase-tagged struct (`prfOutput`). Every other request
decodes into the matching `wavewalletdk.*Request` DTO, so it follows the
PascalCase rule.

## Lifecycle

```go
// Start boots the daemon and blocks until gRPC is serving (or errors).
// Call it off the main thread.
func Start(cfgJSON string) error

// Stop tears down the daemon. Idempotent; resets the singleton so a host can
// restart after the OS suspends/resumes the app.
func Stop() error
```

- **Synchronous.** `Start` blocks until the embedded daemon's private gRPC
  transport reports ready, then returns; the daemon keeps running on its own
  goroutine. The host must call it on a background thread
  (`withContext(Dispatchers.IO)` / a Swift `Task`).
- **Singleton-guarded** by a four-state lifecycle (stopped / starting / started
  / stopping): a second `Start` before `Stop` returns an error instead of
  booting a second daemon. A `Stop` that races an in-progress `Start` cancels
  the boot, and that `Start` tears down any client it produced rather than
  leaking it. A panic in the boot path is recovered into the returned error; an
  unrecovered panic would kill the host process across the boundary.
- The wrapper owns an internal `context.Context` created in `Start` and
  cancelled by `Stop`, so the mobile API never has to express a `context.Context`
  and in-flight RPCs / subscriptions unwind on shutdown.

### Streaming

```go
// Subscribe opens a wallet activity stream and returns a pull handle.
func Subscribe(reqJSON []byte) (*Subscription, error)

func (s *Subscription) Next() ([]byte, error) // blocks; io.EOF at clean end
func (s *Subscription) Close() error          // cancels a blocked Next
```

The host loops `next()` on a background thread; this maps directly to a Kotlin
`Flow` or a Swift `AsyncStream`. `Stop` also ends every open subscription.

## Config

`Start` takes a JSON string decoded into a flat, scalar-only config
(`config.go`, `mobileConfig`). It is the JSON-friendly subset of
`wavewalletdk.Config`: it omits `DaemonConfig *waved.Config` and
`LogWriter io.Writer`, expresses durations as integer seconds, and follows the
same enable-only / non-empty overlay semantics (the zero value defers to the
`wavewalletrpc` build defaults). An empty string boots from
`wavewalletdk.DefaultConfig`.

Empty `server_address` and `swap_server_address` values select the endpoint
for the configured network and transport. They don't disable either service.
Set the fields explicitly when a mobile host needs a custom or local endpoint.

```json
{
  "data_dir": "/data/user/0/<app>/files/wavewalletdk",
  "network": "regtest",
  "server_address": "10.0.2.2:9000",
  "server_insecure": true,
  "wallet_type": "lwwallet",
  "wallet_esplora_url": "http://10.0.2.2:3000"
}
```

(`10.0.2.2` is the Android emulator's alias for the host loopback.)

## Building the bindings

Prerequisites:

- **gomobile** at a recent version: `go install golang.org/x/mobile/cmd/{gomobile,gobind}@latest`.
- **Android SDK + NDK** (the `android` CLI installs them under
  `~/Library/Android/sdk`): `android sdk install platform-tools ndk/<version>`.
- A **modern JDK** (8 is too old; use 17+). Point `JAVA_HOME` at it.
- Build with `GOPATH` *not* equal to `GOROOT` (gomobile and the linter both
  dislike `GOPATH==GOROOT`).

```bash
make mobile-android       # -> sdk/wavewalletdk/mobile/build/android/Wavewalletdk.aar
make mobile-ios           # -> sdk/wavewalletdk/mobile/build/ios/Wavewalletdk.xcframework
make mobile target=all
```

These call [`gen_bindings.sh`](../sdk/wavewalletdk/mobile/gen_bindings.sh), which
runs `gomobile bind` with `-tags="mobile wavewalletrpc swapruntime"`,
`-androidapi 21`, and a 16KB-page-size `-extldflags` (for newer Android
devices). The generated Java package is `engineering.lightning.wavewalletdk`.

> The embedded build pulls in `btcwallet`/`neutrino`/`lnd`, so the `.aar` is
> large. Measure binary size before shipping.

### Publishing

The commands above are for local development. On a `v*` release tag, CI builds
both bindings and attaches `Wavewalletdk.aar` and
`Wavewalletdk.xcframework.tar.gz` to the GitHub release automatically (see the
`publish` job in [`mobile-bindings.yml`](../.github/workflows/mobile-bindings.yml)).
The browser wasm runtime is published separately to the hosted bucket via
`make wasm-publish`. Both flows are documented in
[`docs/release.md`](release.md#publishing-the-wasm-and-mobile-binding-assets).

## Sample app

The sample Android app (and, later, iOS app + idiomatic Kotlin/Swift wrappers)
lives in a separate repo: **[lightninglabs/wavelength-mobile](https://github.com/lightninglabs/wavelength-mobile)**.
It consumes the `.aar` this repo produces via a `scripts/fetch-aar.sh` that
calls `make mobile-android` against a sibling `wavelength` checkout. See that
repo's README and `docs/signet.md` for the end-to-end run (emulator, signet
sync).

SDK component install (one time), for reference:

```bash
android sdk install platform-tools emulator platforms/android-36 \
  build-tools/36.1.0 ndk/29.0.14206865 \
  system-images/android-36/google_apis/arm64-v8a
```

From Kotlin, the generated package exposes the free functions as static methods
on a `Mobile` class (package `engineering.lightning.wavewalletdk.mobile`):

```kotlin
import engineering.lightning.wavewalletdk.mobile.Mobile

// Start blocks until gRPC is serving, so run it off the main thread.
withContext(Dispatchers.IO) { Mobile.start(configJson) }

val balanceJson = Mobile.balance()        // ByteArray of wavewalletdk.Balance JSON
val confirmed = Mobile.confirmedBalanceSat() // Long, no JSON decode

// Streaming as a Flow:
fun walletActivity() = flow {
    val sub = Mobile.subscribe(ByteArray(0))
    try {
        while (true) emit(String(sub.next())) // io.EOF throws -> loop ends
    } finally {
        sub.close()
    }
}.flowOn(Dispatchers.IO)
```

No interfaces to implement. A thin wrapper that maps these throwing calls to
Swift `async` is the iOS sibling; both are optional — the generated API is
callable as-is.

## Not yet wired

- A host-driven `ProcessNextMsg` mailbox pump (issue #713 open question #2)
  cooperative with OS suspension. The package currently relies on the embedded
  daemon's always-on background ingress loop.
- Reference Kotlin/Swift ergonomic wrappers (sketched above, not shipped).
