# daemonrpc

## Purpose

Daemon gRPC API definitions for the single `DaemonService`: wallet lifecycle
(GenSeed/InitWallet/UnlockWallet, with recovery-scan fields), boarding and
VTXO balance/send/refresh/leave/on-chain operations, boarding-UTXO sweeps,
round FSM queries (including streaming `WatchRounds`), OOR session tracking,
fee estimation/history, unilateral-exit (`Unroll`) status, and durable vHTLC
on-chain recovery (`ArmVHTLCRecovery`/`EscalateVHTLCRecovery`/
`CancelVHTLCRecovery`). Proto source: `daemonrpc/daemon.proto`.

## Key Types

- `WalletNotReadyError` / `WalletNotReadyStateError` / `IsWalletNotReadyError`
  / `WalletNotReadyState` (`errors.go`) — the only hand-written file in this
  package. Builds/inspects a structured `FailedPrecondition` gRPC error
  carrying a `WALLET_NOT_READY` `ErrorInfo` reason plus a `wallet_state`
  metadata key. The string constants
  (`WalletNotReadyStateNone/Locked/Syncing/Unknown`) are the wire-level
  mirror of the proto `WalletState` enum.

## Relationships

- **Depends on**: nothing internal (proto definitions plus `errors.go`,
  which only wraps `google.golang.org/grpc/status` and `genproto/errdetails`).
- **Depended on by**: `darepod` (implements `DaemonService`, raises
  `WalletNotReadyError`/`WalletNotReadyStateError`), `cmd/darepocli` (uses
  generated clients), `swapwallet` and `sdk/swaps` (call
  `IsWalletNotReadyError`/`WalletNotReadyState` to gate swap operations on
  wallet lifecycle state).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- Keep the `WalletNotReadyState*` string constants in `errors.go` in sync
  with the proto `WalletState` enum. Callers match on these stable strings,
  not the enum's numeric value, so adding or renaming a `WalletState` value
  without a matching constant silently breaks downstream wallet-state
  detection in `swapwallet`/`sdk/swaps`/`cmd/darepocli`.
- RPC fields are additive-only: never renumber or repurpose an existing
  field number. When retiring a field (e.g. the boarding-forfeit/sweep
  key/delay fields moved to per-round `roundpb.ClientBatchInfo`, or the
  `max_boarding_amount` -> `max_vtxo_amount` rename), leave a `// Field
  number N is unused: ...` comment rather than reusing the number.
