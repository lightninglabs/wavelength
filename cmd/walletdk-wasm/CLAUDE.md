# cmd/walletdk-wasm

## Purpose

Browser entry point that compiles the embedded `darepod` wallet runtime to
`js/wasm` and exposes it to page JavaScript as a single `walletdkCall(method,
req)` function returning a Promise, so a web app can run the daemon, swap,
and OOR machinery in-process in the browser VM with no separate gateway.

## Key Types

- `main` (js/wasm build) — installs `walletdkCall` on the JS global object,
  fires a `walletdk-ready` `CustomEvent`, then parks the goroutine forever so
  exported callbacks stay live for the page's lifetime.
- `main` (native stub build) — prints a build-tag hint to stderr and exits 1;
  keeps `go build ./...` green on toolchains that are not `js/wasm`.
- `walletCall` — the single JS dispatch point; switches on a method name
  string and forwards to the matching `sdk/walletdk/mobile` verb (`start`,
  `stop`, `getInfo`, `balance`, `createWallet`, `prepareSend`, `exit`,
  `subscribe`, …).
- `subscriptionHandle` — wraps a `mobile.Subscription` as a JS object with
  `next()`/`close()` methods, since a Go channel cannot cross into
  JavaScript.
- `promise` — runs a Go closure on a fresh goroutine and resolves/rejects a JS
  `Promise`, recovering panics so they never kill the Go/wasm runtime.

## Relationships

- **Depends on**: `sdk/walletdk/mobile` (the JSON facade this bridge
  dispatches every verb to; it never calls `sdk/walletdk` directly, so it
  cannot drift from the mobile bindings' behavior).
- **Depended on by**: nothing in-repo — it is a leaf `cmd/` binary built
  directly by browser tooling via `GOOS=js GOARCH=wasm go build`.

## Invariants

- Real build requires `//go:build js && wasm` plus `-tags "mobile
  walletdkrpc swapruntime"`; without those tags the daemon and swap RPCs are
  compiled out of `sdk/walletdk/mobile` and every verb would fail at
  runtime, so `main.go`'s tag and the build command's `-tags` must stay in
  sync.
- `stub.go` (`//go:build !js || !wasm`) exists purely so `go build ./...`
  succeeds on native toolchains; it must never gain real logic — real logic
  belongs in `main.go` behind the js/wasm tag.
- A browser has no `$HOME`, so `startConfig` injects a fixed
  `browserDataDir` ("/darepo") when the caller omits `data_dir`; removing
  that default breaks `mobile.Start`'s config validation under
  `wasm_exec.js`.
- Every exported `js.Func` callback (e.g. in `subscriptionHandle`, the
  `promise` executor) must be `Release()`d once it can no longer fire, or it
  leaks a Go callback handle for the lifetime of the page.
- This bridge is a thin dispatcher only: it must not call into
  `sdk/walletdk` or any daemon/actor code directly. Add new capabilities to
  `sdk/walletdk/mobile` first, then add a `case` here.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
