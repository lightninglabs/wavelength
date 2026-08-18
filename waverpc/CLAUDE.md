# waverpc

## Purpose

Daemon gRPC API definitions for wallet, boarding, round, OOR, unroll, and
VHTLC-recovery operations. Proto source: `waverpc/daemon.proto`. Generated
gRPC, REST-gateway, and mailbox-RPC stubs plus one hand-written helper file
(`errors.go`) for structured wallet-lifecycle errors.

`DaemonService` also carries a **delegated-signing family** —
`SignReceiveAuthMessage`, `SignReceiveAuthMessageCompact`, `SignOORCustomInput`,
`SignVTXOForfeit`, `SignOutSwapHtlcAck`, `SignCreditAccountAuthorization` —
through which higher layers (`sdk/swaps`, `swapclientserver`, `swapwallet`)
obtain signatures from keys the daemon holds without ever handling those keys
themselves. The request shapes are deliberately narrow rather than
"sign these bytes", so the daemon can bound what it authorizes:
`SignVTXOForfeit` and `SignOORCustomInput` describe the transaction being
signed, and `SignOutSwapHtlcAck` / `SignCreditAccountAuthorization` carry the
authorization's committed fields (expiry, nonce, account key) alongside the
digest so the daemon can refuse one that is not its own to grant — see
[waved/CLAUDE.md](../waved/CLAUDE.md) for the checks each applies.
`SignReceiveAuthMessage`(`Compact`) is the exception and is scoped by key
instead: it signs a raw message under the per-swap receive-auth key selected by
`payment_hash`, not under the daemon identity key.

## Key Types

- `DaemonServiceClient` / `DaemonServiceServer` — Generated gRPC client and
  server interfaces for the daemon API.
- `DaemonServiceMailboxClient` / `DaemonServiceMailboxServer` — Generated
  mailbox-RPC client/server stubs (via `protoc-gen-mailboxrpc`).
- `WalletNotReadyError(msg)` / `WalletNotReadyStateError(msg, state)` — Build a
  structured `FailedPrecondition` gRPC error carrying a stable `ErrorInfo`
  reason (`WalletNotReadyReason`) and optional `wallet_state` metadata.
- `IsWalletNotReadyError(err)` / `WalletNotReadyState(err)` — Match and unpack
  the structured error produced above; callers should key off these instead of
  matching on message text.

## Relationships

- **Depends on**: `mailbox/rpc` (mailbox-RPC runtime types used by the
  generated mailbox stubs), `google.golang.org/genproto/googleapis/rpc/errdetails`
  and `google.golang.org/grpc` (structured errors in `errors.go`),
  `grpc-gateway/runtime` (REST gateway in `daemon.pb.gw.go`).
- **Depended on by**: `waved` (implements `DaemonServiceServer`),
  `cmd/wavecli` and `rpc/restclient` (CLI/REST clients), `sdk/ark`,
  `sdk/swaps`, `sdk/wavewalletdk`, `swapclientserver`, `swapwallet` (typed clients
  for daemon RPCs).

## Invariants

- **Never edit generated code** (`daemon.pb.go`, `daemon_grpc.pb.go`,
  `daemon.pb.gw.go`, `daemon_mailboxrpc.pb.go`) — regenerate via `make rpc`
  after editing `daemon.proto` or `daemon.yaml`.
- `errors.go` is hand-written and not regenerated; callers must match wallet
  lifecycle errors via `IsWalletNotReadyError`/`WalletNotReadyState`, never by
  parsing the error message string.
