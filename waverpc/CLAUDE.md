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
- `NewReceiveScriptRequest.idempotency_key` is an API-level contract, not just
  an implementation detail: the key namespace is **global to one daemon**, so
  callers sharing a daemon must prefix keys with an application or tenant
  identity or they will be handed each other's receive scripts. An empty key
  keeps the legacy allocate-a-fresh-script behavior; repeating a non-empty key
  with a *different* label is rejected rather than silently reallocated.
- `SendOORRequest.idempotency_key` is an API-level contract on the same
  footing. A non-empty key permanently binds one caller intent to one OOR
  session in the daemon's immutable `oor_dispatch_attempts` table; a terminal
  failure, or a later incoming self-transfer reusing the same session id, never
  releases the key. Retrying a bound key with a *changed* recipient set (count,
  amount, or pkScript) is rejected with `AlreadyExists` instead of sent again —
  only an exact replay returns the original session, and recipient *reordering*
  still counts as exact. Callers must therefore treat the key as covering the
  whole recipient multiset, not just "this send".
- `SendOORRequest.existing_only` turns the call into a read-only probe: the
  daemon reconciles the key, never selects inputs, and never admits a new
  transfer. It requires a non-empty `idempotency_key` and answers `NotFound`
  when no winner exists.
- `SendOORResponse.status` is a four-value string, and the `.proto` comment
  lists only three — treat this as the full set:
  - `submitted` — fresh admission, or exact replay of a live/completed send.
  - `failed` — replay found the keyed session's outgoing lifecycle already
    terminally failed. No recipient outpoints are returned because none exist.
  - `pending` — `existing_only` only: the registry has accepted an admission
    whose dispatch-attempt row is not yet committed, so the session id is
    stable but its outpoints are not yet provable.
  - `preview` — `dry_run`. `session_id` is empty.
  A replay of a *legacy* binding (one backfilled by migration 18, carrying no
  canonical request data) reports `submitted` with the stable session id and an
  empty `recipient_outpoints`: the daemon can prove the send happened but not
  where it landed, so clients must not read an empty list as "no outputs".

## Deep Docs

- [docs/oor_subsystem.md](../docs/oor_subsystem.md) — OOR session model and the
  dispatch-attempt replay guarantees behind the `SendOOR` contract above.
- [docs/idempotent-receive-scripts-execplan.md](../docs/idempotent-receive-scripts-execplan.md)
  — Retry-safe `NewReceiveScript` allocation: durable identity, exact replay,
  and expiry renewal.
- [waved/CLAUDE.md](../waved/CLAUDE.md) — Server-side implementation of these
  RPCs.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
