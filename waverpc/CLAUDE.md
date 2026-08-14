# waverpc

## Purpose

Daemon gRPC API definitions for wallet, boarding, round, OOR, unroll,
VHTLC-recovery, and swap-signing operations. Proto source:
`waverpc/daemon.proto`. Generated gRPC, REST-gateway, and mailbox-RPC stubs
plus one hand-written helper file (`errors.go`) for structured
wallet-lifecycle errors.

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
- **The identity-key signing RPCs exist so the key never leaves the daemon.**
  `SignReceiveAuthMessage[Compact]`, `ReceiveAuthECDH`, `SignOutSwapHtlcAck`,
  and `SignCreditAccountAuthorization` all sign a caller-supplied digest or
  message with the daemon's identity key; the swap SDK holds no raw key
  material. Adding another follows the same three steps and none is optional:
  the RPC in `daemon.proto`, its gateway route in `daemon.yaml`, **and** a
  macaroon grant in `waved.newWavedRPCPermissions` (the signing RPCs sit under
  `entitySwap:write` / `entityWallet`) — a new RPC without the grant is
  reachable only by an unrestricted macaroon. A REST caller additionally needs
  a hand-written wrapper in `rpc/restclient`, which `make rpc` does not
  generate.
