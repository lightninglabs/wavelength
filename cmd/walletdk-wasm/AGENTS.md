# cmd/walletdk-wasm

## Purpose

`js/wasm` build target that exposes the embedded `sdk/walletdk` wallet
runtime to browser JavaScript. It installs a single global
`walletdkCall(method, req)` function backed by `syscall/js`, so the daemon,
swap, and OOR (out-of-round) machinery all run in-process inside the one
browser VM — there is no separate gateway process or WebSocket hop.

## Key Types

- `main` (main.go) — installs `walletdkCall` on `js.Global()`, fires a
  `walletdk-ready` `CustomEvent`, then blocks forever (`select {}`) so the
  Go runtime and its exported callbacks stay alive for the page lifetime.
- `main` (stub.go) — non-js/wasm build (`!js || !wasm`); prints a build-tag
  hint to stderr and exits 1, keeping `go build ./...` green on native
  toolchains.
- `walletCall` — the single JS entry point; switches on a method-name
  string (`start`, `stop`, `getInfo`, `balance`, `createWallet`,
  `unlockWallet`, `deposit`, `receive`, `prepareSend`, `sendPrepared`,
  `list`, `exit`, `exitStatus`, `getExitPlan`, `sweepWallet`,
  `confirmedBalanceSat`, `pendingInboundSat`, `walletReady`, `isRunning`,
  `subscribe`) and dispatches to the matching `sdk/walletdk/mobile` verb.
- `subscriptionHandle` — wraps a pull-based `mobile.Subscription` as a JS
  object with `next()`/`close()` methods; owns the underlying `js.Func`
  callbacks and releases them on `close()`.
- `promise` — runs a Go closure on a fresh goroutine and surfaces the
  result as a JS `Promise`, recovering panics into a rejection so a bug in
  one call never kills the shared Go runtime/page.

## Relationships

- **Depends on**: `sdk/walletdk/mobile` (the JSON facade this bridge
  adapts — every verb is a JSON-bytes-in/JSON-bytes-out call shared
  verbatim with the gomobile bindings, so this package never touches
  `sdk/walletdk.Client` directly and cannot drift from the gomobile
  surface).
- **Depended on by**: nothing in-repo (top-level binary); consumed
  externally by browser/web frontends that load the compiled `.wasm`
  artifact plus `wasm_exec.js`.
- **Sends**: N/A — this is a WASM bridge that exposes a synchronous
  JS-callable function (`walletdkCall`) returning Promises, not an actor
  participant. It forwards JSON payloads into `mobile.Start` /
  `mobile.GetInfo` / `mobile.CreateWallet` / etc., which drive the
  embedded daemon over an in-process `bufconn` gRPC transport (see
  `sdk/walletdk` CLAUDE.md) — no `Tell`/`Ask`/actor messages originate
  here.
- **Receives**: ← JS caller: `walletdkCall(method, req)` invocations from
  browser code; each `req` is a plain JS object JSON-stringified before
  crossing into Go.

## Invariants

- Build tags gate everything: `main.go` is `js && wasm` only, and the real
  binary additionally requires `-tags "mobile walletdkrpc swapruntime"`
  (`GOOS=js GOARCH=wasm go build -tags "mobile walletdkrpc swapruntime"
  ./cmd/walletdk-wasm`). Without those tags the `mobile` facade's wallet
  methods return `ErrWalletRPCUnavailable` instead of doing real work.
- `main` never returns (`select {}`): the exported `js.Func` callbacks
  (`walletdkCall`, and per-subscription `next`/`close`) are only reachable
  while the Go scheduler keeps running, so returning from `main` would
  tear down every live callback out from under the page.
- Every `js.Func` created for a one-shot use (the `Promise` executor in
  `promise`) must be `Release()`d immediately after use, and long-lived
  ones (`subscriptionHandle`'s `next`/`close`) must be released in
  `close()`. Leaking a `js.Func` handle leaks Go-side memory for the
  lifetime of the page since the JS GC cannot reclaim it.
- There is no `$HOME` under `wasm_exec.js`, so `startConfig` always
  injects `browserDataDir` (`/darepo`) when the caller's request omits
  `data_dir`; otherwise the embedded daemon's config validation calls
  `os.UserHomeDir` while expanding the default `~/.darepod` and aborts
  startup with "$HOME is not defined".
- A panic inside any `promise`-wrapped closure is recovered and turned
  into a Promise rejection (`"walletdk panic"`) rather than propagating,
  because an uncaught panic in a goroutine would crash the whole Go/WASM
  runtime and take down the page, not just the one call.
- The `mobile` facade is the single source of truth for verb behavior,
  shared with the gomobile bindings — new wallet verbs belong in
  `sdk/walletdk/mobile`, not as bespoke logic in this bridge.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
- [sdk/walletdk/CLAUDE.md](../../sdk/walletdk/CLAUDE.md) — The wallet SDK
  facade this bridge sits above.
</content>
