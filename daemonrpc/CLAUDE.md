# daemonrpc

## Purpose

Daemon gRPC API definitions for wallet operations and round queries. Proto
source: `daemonrpc/daemon.proto`.

## Key Types

- `RoundState` enum — Client-visible FSM state for a round participation
  lifecycle. Notable values:
  - `ROUND_STATE_REGISTRATION_SENT` (intent sent, awaiting server admission
    and seal-time quote)
  - `ROUND_STATE_QUOTE_RECEIVED = 15` — Client has received the server's
    `JoinRoundQuote` and is deciding whether to accept or reject it. Inserted
    between `REGISTRATION_SENT` and `JOINED` as Phase-2 of the #270 seal-time
    fee handshake.
  - `ROUND_STATE_JOINED` (quote accepted, awaiting commitment tx)

## Relationships

- **Depends on**: nothing (proto definitions).
- **Depended on by**: `darepod` (implements services), `cmd/darepocli` (uses
  generated clients), `rpc/roundpb` (generated from related proto).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- The `Board` RPC triggers `IntentRequested` (not `RegistrationRequested`) in
  the round FSM — the event was renamed as part of the #270 handshake.
