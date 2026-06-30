# cmd/walletdk-wasm

## Purpose

WebAssembly binary entry point for the walletdk embedded daemon. The
real `main` (compiled with `GOOS=js GOARCH=wasm`) boots the
`sdk/walletdk/mobile` facade and exposes it to JavaScript via the
WASM/JS bridge. A stub `main` in `stub.go` makes the package
build-clean under native toolchains and explains how to build the real
WASM binary.

## Key Types

- `main` — Entry point; only meaningful as a `js/wasm` target.

## Relationships

- **Depends on**:
  - `sdk/walletdk/mobile` (gomobile/WASM facade over the embedded daemon)
- **Depended on by**: nothing at the Go level; the compiled `.wasm`
  binary is loaded by JavaScript in the host web application.
- **Sends**: nothing (passes control to the mobile facade).
- **Receives**: nothing at the Go level.

## Invariants

- Only the `js/wasm` build is meaningful. Native builds compile via
  `stub.go` only to keep `go build ./...` green.
- Build: `GOOS=js GOARCH=wasm go build -tags 'mobile walletdkrpc swapruntime' ./cmd/walletdk-wasm`

## Deep Docs

- [sdk/walletdk/mobile/CLAUDE.md](../../sdk/walletdk/mobile/CLAUDE.md)
  — The WASM/mobile facade this command boots.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
