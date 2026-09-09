# waverpc

## Purpose

Daemon gRPC API definitions for wallet, boarding, round, OOR, unroll, and
VHTLC-recovery operations, plus the `Sign*` family through which other
subsystems borrow the daemon identity key (`SignReceiveAuthMessage[Compact]`,
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
- `ListVTXOsRequest` has two status selectors and they are not
  interchangeable at the default. `status_filter` (singular, legacy) is
  equivalent to one entry in the repeated `statuses` field. When **both** are
  unset the response is the *inventory* set — every VTXO except
  `VTXO_STATUS_FORFEITED` and `VTXO_STATUS_SPENT` — not "all statuses" as the
  older comment claimed. `VTXO_STATUS_PENDING_ROUND` entries are returned only
  when explicitly listed. Callers that want a spendable-balance view must
  filter to live entries themselves; the default deliberately includes
  non-spendable inventory so a VTXO never silently vanishes from a listing.
- `ServerInfo.vtxo_confirmations` is the depth at which new round VTXOs become
  spendable off-chain, and is **independent of** `min_confirmations`, which
  governs the on-chain boarding inputs a new round consumes. Zero means the
  operator does not advertise the split field; consumers fall back to
  `min_confirmations` (see `lib/types.OperatorTerms.VTXOTargetConfirmations`).
- `NewReceiveScriptRequest.idempotency_key` is an API-level contract, not just
  an implementation detail: the key namespace is **global to one daemon**, so
  callers sharing a daemon must prefix keys with an application or tenant
  identity or they will be handed each other's receive scripts. An empty key
  keeps the legacy allocate-a-fresh-script behavior; repeating a non-empty key
  with a *different* label is rejected rather than silently reallocated.
