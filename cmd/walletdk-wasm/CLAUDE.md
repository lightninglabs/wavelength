# cmd/walletdk-wasm

## Purpose

Browser WASM entry point that exposes the embedded walletdk daemon to
JavaScript. It is a thin `syscall/js` adapter over the `sdk/walletdk/mobile`
JSON facade — the daemon, swap, and OOR machinery all run in-process in the
browser VM, with no separate gateway.

## Key Types

- `main` (`js && wasm` build tag, `main.go`) — Installs a single global JS
  function `walletdkCall`, fires a `walletdk-ready` `CustomEvent`, then parks
  the goroutine so exported callbacks stay live for the page's lifetime.
- `walletCall` — Single dispatch point mapping a JS method-name string
  (`start`, `stop`, `getInfo`, `createWallet`, `prepareSend`, `subscribe`,
  ...) onto the corresponding `sdk/walletdk/mobile` facade call.
- `subscriptionHandle` — Wraps a `mobile.Subscription` as a JS object with
  `next()`/`close()` methods for pull-based streaming.
- `promise` — Runs facade work on a goroutine and resolves/rejects a JS
  `Promise`; recovers panics so they never crash the Go runtime.
- `main` (`!js || !wasm` build tag, `stub.go`) — Keeps `go build ./...` green
  on native toolchains; prints the required build command and exits 1.

## Relationships

- **Depends on**: `sdk/walletdk/mobile` (the JSON facade: `Start`, `Stop`,
  `GetInfo`, `Balance`, `Status`, `CreateWallet`, `UnlockWallet`,
  `OpenWalletFromPasskey`, `Deposit`, `Receive`, `PrepareSend`,
  `SendPrepared`, `List`, `Exit`, `ExitStatus`, `GetExitPlan`, `SweepWallet`,
  `ConfirmedBalanceSat`, `PendingInboundSat`, `WalletReady`, `IsRunning`,
  `Subscribe`) — the same facade shared with the gomobile bindings.
- **Depended on by**: nothing in-repo (top-level binary); consumed
  externally by browser JS after building with
  `GOOS=js GOARCH=wasm go build -tags "mobile walletdkrpc swapruntime"`.

## Invariants

- Never call into `sdk/walletdk.Client` directly from this package — always
  go through the `sdk/walletdk/mobile` facade, so the WASM bridge cannot
  drift from the gomobile bindings surface.
- `browserDataDir` (`/darepo`) is injected into the start config whenever the
  caller omits `data_dir`: the daemon's default `~/.darepod` expansion via
  `os.UserHomeDir` fails under `wasm_exec.js` because there is no `$HOME`.
- `promise`'s executor `js.Func` must be `Release`d immediately after the
  `Promise` is constructed (the constructor invokes it synchronously and
  never again) — otherwise every wallet call leaks a Go callback handle.
- `subscriptionHandle` releases both its `next` and `close` `js.Func`
  callbacks on `close()` to avoid leaking handles for the subscription's
  lifetime.
