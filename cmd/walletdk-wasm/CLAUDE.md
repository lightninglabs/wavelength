# cmd/walletdk-wasm

## Purpose

Browser entry point that exposes the embedded walletdk runtime to JavaScript
via `syscall/js`. It is a thin adapter over the `sdk/walletdk/mobile` JSON
facade: every JS verb marshals a request object to JSON, calls the matching
`mobile.*` function, and resolves a JS Promise with the JSON response parsed
back into a JS value. The daemon, swap, and OOR machinery all run in-process
in the one browser VM — there is no separate gateway process.

Only buildable for `js/wasm`:

```
GOOS=js GOARCH=wasm go build -tags "mobile walletdkrpc swapruntime" ./cmd/walletdk-wasm
```

`stub.go` (`//go:build !js || !wasm`) provides a native-arch `main` that
exits with a usage message, so `go build ./...` stays green on normal
toolchains without needing wasm-specific build logic in the module graph.

## Key Types

- `main()` (main.go, `js && wasm`) — Registers `window.walletdkCall` as the
  single JS entry point, dispatches a `walletdk-ready` `CustomEvent`, then
  blocks forever (`select {}`) so the Go runtime and its exported callbacks
  stay alive for the lifetime of the page.
- `walletCall(_, args)` — Dispatches on a method-name string (`"start"`,
  `"stop"`, `"getInfo"`, `"balance"`, `"status"`, `"createWallet"`,
  `"unlockWallet"`, `"openWalletFromPasskey"`, `"deposit"`, `"receive"`,
  `"prepareSend"`, `"sendPrepared"`, `"list"`, `"exit"`, `"exitStatus"`,
  `"getExitPlan"`, `"sweepWallet"`, `"confirmedBalanceSat"`,
  `"pendingInboundSat"`, `"walletReady"`, `"isRunning"`, `"subscribe"`) to the
  corresponding `mobile.*` facade call and returns a JS Promise.
- `promise(fn)` — Runs `fn` on a fresh goroutine, recovers any panic so it
  can never crash the Go/wasm runtime, and resolves/rejects a JS `Promise`
  with the result. Releases its executor `js.Func` immediately after
  `Promise.New` returns (the executor runs synchronously and is never
  called again), so no callback handle is leaked per call.
- `subscriptionHandle(sub)` — Wraps a pull-based `mobile.Subscription` as a
  JS object with `next()`/`close()` methods. It owns two `js.Func` values
  (`nextFn`, `closeFn`) and releases both in `close()` — the caller must
  call `close()` or these handles leak for the page's lifetime.
- `startConfig(req)` — Injects a browser-safe `data_dir` (`/darepo`, see
  `browserDataDir`) into the start request when the caller didn't set one,
  because there is no `$HOME` for the daemon's `~` expansion in a browser.

## Relationships

- **Depends on**: `sdk/walletdk/mobile` (the JSON facade shared with the
  gomobile bindings — this bridge never calls `walletdk.Client` directly so
  it cannot drift from the mobile API); `syscall/js` and `io` (standard
  library) for JS interop and `io.EOF` subscription end-of-stream detection.
- **Depended on by**: nothing in-repo (top-level wasm binary, consumed by
  browser-side JS that loads the compiled `.wasm` module).

## Invariants

- Never call into `walletdk.Client` (or any other internal wallet package)
  directly from this bridge — always go through `sdk/walletdk/mobile` so the
  wasm, gomobile, and any future bindings share one JSON contract.
- Every `js.Func` created for a callback (`promise`'s executor,
  `subscriptionHandle`'s `nextFn`/`closeFn`) must be `Release()`d exactly
  once on every code path, or it leaks for the lifetime of the page — the
  wasm heap has no GC visibility into JS-held Go callback references.
- `main` must never return; `select {}` keeps exported functions
  (`walletdkCall`) reachable. Returning from `main` tears down the Go
  runtime and invalidates every JS-side callback.
- `stub.go`'s build tag (`!js || !wasm`) must stay the exact complement of
  main.go's (`js && wasm`) so exactly one `main` is compiled for any
  `GOOS`/`GOARCH` pair.

## Deep Docs

- [sdk/walletdk/CLAUDE.md](../../sdk/walletdk/CLAUDE.md) — walletdk SDK layer.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
