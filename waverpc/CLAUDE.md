# waverpc

## Purpose

Daemon gRPC API definitions for wallet, boarding, round, OOR, unroll,
VHTLC-recovery, and Ark-channel funding operations, plus the `Sign*` family
through which other subsystems borrow the daemon identity key
(`SignReceiveAuthMessage[Compact]`,
`SignOORCustomInput`, `SignVTXOForfeit`, `SignOutSwapHtlcAck`,
`SignCreditAccountAuthorization`). Proto source: `waverpc/daemon.proto`.
Generated gRPC, REST-gateway, and mailbox-RPC stubs plus one hand-written
helper file (`errors.go`) for structured wallet-lifecycle errors.

## Key Types

- `DaemonServiceClient` / `DaemonServiceServer` — Generated gRPC client and
  server interfaces for the daemon API.
- `DaemonServiceMailboxClient` / `DaemonServiceMailboxServer` — Generated
  mailbox-RPC client/server stubs (via `protoc-gen-mailboxrpc`).
- `MacaroonServiceClient` / `MacaroonServiceServer` — Local authenticated
  credential provisioning and active-permission discovery. The daemon does
  not register this service on its operator mailbox transport.
- `WalletNotReadyError(msg)` / `WalletNotReadyStateError(msg, state)` — Build a
  structured `FailedPrecondition` gRPC error carrying a stable `ErrorInfo`
  reason (`WalletNotReadyReason`) and optional `wallet_state` metadata.
- `IsWalletNotReadyError(err)` / `WalletNotReadyState(err)` — Match and unpack
  the structured error produced above; callers should key off these instead of
  matching on message text.
- `ArkChannelOORPreparationStatus` — Reconciliation state returned by
  `LookupPreparedArkChannelOOR`: `UNSPECIFIED`, `ABSENT`, `PENDING`,
  `PREPARED`, or `ACCEPTED`.

## Relationships

- **Depends on**: `rpc/arkchannelrpc` (`daemon.proto` imports shared channel
  terms, VTXO bindings, and recovery messages), `mailbox/rpc` (mailbox-RPC
  runtime types used by the
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
- `NewReceiveScriptRequest.idempotency_key` is an API-level contract, not just
  an implementation detail: the key namespace is **global to one daemon**, so
  callers sharing a daemon must prefix keys with an application or tenant
  identity or they will be handed each other's receive scripts. An empty key
  keeps the legacy allocate-a-fresh-script behavior; repeating a non-empty key
  with a *different* label is rejected rather than silently reallocated.
- `NewReceiveScriptRequest.identity_key` registers the daemon's durable
  identity key for a restart-stable protocol destination, such as an Ark
  channel cooperative-close payout. It is mutually exclusive with
  `idempotency_key`; combining the two returns `InvalidArgument`.
- Ark channel funding uses five state-changing or reconciliation RPCs.
  `PrepareArkChannelOOR` reserves liquidity without releasing signatures,
  `LookupPreparedArkChannelOOR` finds the same deterministic preparation,
  and `ValidatePreparedArkChannelOOR` checks its exact terms and binding.
  Exactly one of `CommitPreparedArkChannelOOR`, after both lnd endpoints
  persist the signed backing, or `AbortPreparedArkChannelOOR`, before that
  point of no return, terminates the reservation. Aborts require a reason.
- `ExportOORRecoveryPackage` separately exports the finalized OOR package
  and round ancestry for one exact output. It grants no ownership and starts
  no watch by itself.
