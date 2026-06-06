# daemonrpc

## Purpose

Daemon gRPC API definitions for wallet operations and round queries, plus
structured error helpers for wallet lifecycle preconditions. Proto source:
`daemonrpc/daemon.proto`.

## Key Types

- `WalletNotReadyError(msg)` — Returns a structured gRPC
  `FailedPrecondition` error. Stable `ErrorInfo.Reason =
  "WALLET_NOT_READY"`, domain `"darepo-client/wallet"`. Callers should
  match the reason, not the human message.
- `WalletNotReadyStateError(msg, state)` — Like `WalletNotReadyError` but
  includes the current wallet lifecycle state in `ErrorInfo.Metadata` under
  the key `"wallet_state"`. State values: `WalletNotReadyStateNone`,
  `WalletNotReadyStateLocked`, `WalletNotReadyStateSyncing`,
  `WalletNotReadyStateUnknown`.
- `IsWalletNotReadyError(err) bool` — Returns true when `err` carries
  `ErrorInfo.Reason == WalletNotReadyReason`.
- `WalletNotReadyState(err) string` — Extracts the `wallet_state` metadata
  from a wallet-not-ready error; empty string when absent.
- `WalletNotReadyReason = "WALLET_NOT_READY"` — Stable error reason
  constant for callers matching structured errors.
- `WalletNotReadyStateKey = "wallet_state"` — Metadata key for state hints.

## Relationships

- **Depends on**: nothing (proto definitions + error helpers).
- **Depended on by**: `darepod` (implements services, emits structured
  wallet errors), `cmd/darepocli` (uses generated clients).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- Wallet error reason and domain are stable API surfaces; only the human
  message may change across versions.
