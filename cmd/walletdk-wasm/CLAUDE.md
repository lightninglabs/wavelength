# cmd/walletdk-wasm

## Purpose

Browser/WASM entry point for the embedded walletdk runtime. Installs a global
`walletdkCall(method, request)` JavaScript function via `syscall/js` and
dispatches each call to the corresponding `sdk/walletdk/mobile` verb, returning
a JS Promise. The daemon, swap, and OOR machinery all run in-process inside the
browser VM with no separate gateway. The bridge never reaches `walletdk.Client`
directly — it always goes through the mobile facade so WASM behavior stays in
sync with the gomobile bindings.

Only compiles with `GOOS=js GOARCH=wasm -tags "mobile walletdkrpc swapruntime"`.
`stub.go` provides an empty build so `go build ./...` succeeds on other targets.

## Key Types

- `main` — installs `walletdkCall` on `js.Global()`, dispatches a
  `walletdk-ready` custom event, then parks the Go runtime so exported
  callbacks remain live for the page lifetime.
- `walletCall` — the single JS entry point; dispatches the method name to the
  corresponding `mobile.*` verb and wraps the result in a Promise.
- `subscriptionHandle` — wraps `*mobile.Subscription` as a JS object with
  `next()` (Promise → next entry JSON or null at EOF) and `close()` methods.

## Relationships

- **Depends on**: `sdk/walletdk/mobile` (all wallet operations proxied through
  the mobile facade).
- **Depended on by**: browser host applications, React Native WASM bridges.
- **Sends**: nothing (invokes mobile facade functions in goroutines).
- **Receives** ← JS host: `walletdkCall(method, request)` invocations.

## Invariants

- `walletCall` is the only JS-exported function; all verb dispatch happens
  inside its switch. Additional `js.Func` values outside `promise()` must be
  released manually — `promise()` already releases its own executor immediately
  after the Promise constructor returns to prevent per-call handle leaks.
- The browser data dir defaults to `/darepo` (injected by `startConfig`).
  Without this override, `os.UserHomeDir` fails under `wasm_exec.js` with
  `"$HOME is not defined"` and aborts start before the wallet boots. Callers
  may override via `data_dir` in the start request.
- This package must not import `sdk/walletdk` directly; all access goes through
  the `mobile` facade to keep the WASM bridge and gomobile bindings in sync.

## Deep Docs

- [sdk/walletdk/mobile/CLAUDE.md](../../sdk/walletdk/mobile/CLAUDE.md) —
  The gomobile facade this package wraps.
- [sdk/walletdk/CLAUDE.md](../../sdk/walletdk/CLAUDE.md) — Underlying Go SDK.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
