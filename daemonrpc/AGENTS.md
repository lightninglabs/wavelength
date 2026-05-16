# daemonrpc

## Purpose

Daemon gRPC API definitions for wallet operations, round queries, and
lifecycle surfaces. Generated protobuf stubs; proto source:
`daemonrpc/daemon.proto`.

## Key Types (notable additions)

- `WalletState` — Tri-state wallet lifecycle enum
  (`WALLET_STATE_NONE`, `WALLET_STATE_LOCKED`, `WALLET_STATE_READY`)
  surfaced in `GetInfoResponse.WalletState` so callers can distinguish
  not-yet-created from locked from ready without string-matching log output.

## Relationships

- **Depends on**: nothing (proto definitions + generated stubs).
- **Depended on by**: `darepod` (implements services), `cmd/darepocli`
  (generated gRPC clients), `sdk/walletdk` (daemon RPC client),
  `cmd/darepocli/internal/gen-devrpc` (proto descriptor introspection).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
