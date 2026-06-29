# cmd/walletdk-wasm

## Purpose

Browser-facing WebAssembly binary that bridges the embedded walletdk runtime
to browser JavaScript. It is a thin `syscall/js` adapter over the
`sdk/walletdk/mobile` JSON facade: every verb takes a JS request object and
resolves a JS response object so the daemon, swap, and OOR machinery all run
in-process in the browser VM with no separate gateway.

Only meaningful as a `js/wasm` target. On native toolchains a stub `main`
compiles in place of the real binary to keep `go build ./...` green.

Build with:
```
GOOS=js GOARCH=wasm go build \
    -tags "mobile walletdkrpc swapruntime" ./cmd/walletdk-wasm
```

## Key Types

- `walletCall` — the single JS entry point registered as `walletdkCall` on
  `js.Global()`. Takes a method name string and an optional request object;
  returns a Promise that resolves with the verb's JSON-decoded JS response or
  rejects with a JS Error.

## Relationships

- **Depends on**: `sdk/walletdk/mobile` (JSON facade — every verb delegates
  here), `syscall/js` (browser bridge).
- **Depended on by**: browser host applications (import the compiled `.wasm`
  and call `walletdkCall`).
- **Sends**: nothing (delegates to `sdk/walletdk/mobile` in-process).
- **Receives**: method dispatch from `walletdkCall` JS calls.

## Invariants

- The facade is the single source of truth shared with the gomobile bindings;
  this bridge never reaches into `walletdk.Client` directly and cannot drift
  from the mobile API.
- On startup, emits a `walletdk-ready` `CustomEvent` on `js.Global()` once
  the entry point is registered.
- The Go runtime parks indefinitely after registration (`select {}`) so
  exported JS callbacks stay live for the lifetime of the page.

## Deep Docs

- [sdk/walletdk/mobile/CLAUDE.md](../../sdk/walletdk/mobile/CLAUDE.md) —
  JSON facade this binary wraps.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
