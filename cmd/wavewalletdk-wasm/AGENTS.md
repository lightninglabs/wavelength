# cmd/wavewalletdk-wasm

## Purpose

Command that compiles the embedded wavewalletdk runtime to a browser WASM binary
and exposes it to JavaScript as a single `wavewalletdkCall(method, req)` entry
point, so the daemon, swap, and OOR machinery run in-process in the browser
VM with no separate gateway.

## Key Types

- `main` (`js && wasm` build) — installs `wavewalletdkCall` on the JS global and
  parks the Go runtime so exported callbacks stay live for the page lifetime.
- `main` (stub, `!js || !wasm`) — keeps `go build ./...` green on native
  toolchains; exits with a message that the real binary needs
  `GOOS=js GOARCH=wasm -tags "mobile wavewalletrpc swapruntime"`.
- `walletCall` — dispatches a JS method name to the matching
  `sdk/wavewalletdk/mobile` verb and returns a JS `Promise`.
- `subscriptionHandle` — wraps a pull-based `mobile.Subscription` as a JS
  object with `next()`/`close()`.

## Relationships

- **Depends on**: `sdk/wavewalletdk/mobile` (the single source of truth JSON
  facade this bridge calls into; it never reaches into `wavewalletdk.Client`
  directly so it cannot drift from the gomobile bindings).
- **Depended on by**: nothing in-repo; this is a leaf binary target consumed
  by a browser build pipeline outside this module.

## Invariants

- Every verb takes a JS request object and resolves/rejects a JS `Promise`;
  never call back into JS synchronously from a goroutine without going
  through `promise`, or a panic can escape and kill the Go runtime.
- `data_dir` must default to `browserDataDir` (`/wavelength`) when unset, because
  the embedded daemon's config validation calls `os.UserHomeDir` for the
  default `~/.waved`, which fails under `wasm_exec.js` (no `$HOME`).
- The `executor` `js.Func` passed to `Promise.New` must be released right
  after construction (the executor runs synchronously), otherwise every
  wallet call leaks a Go callback handle for the life of the page.
- Build tags must stay in sync between `main.go` (`js && wasm`) and
  `stub.go` (`!js || !wasm`) so exactly one `main` compiles per target.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map
