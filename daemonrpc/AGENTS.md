# daemonrpc

## Purpose

Daemon gRPC API definitions for wallet operations and round queries. Proto
source: `daemonrpc/daemon.proto`.

## Key RPCs and Types

- `NewReceiveScript` — Allocates a fresh wallet key and registers a receive script with the indexer. Renamed from `NewOORReceiveScript` to reflect generalized receive (not OOR-specific).
- `LeaveVTXOs` — Queues one or more VTXOs for cooperative leave (off-board to on-chain). Accepts `selection` (specific outpoints or all), `default_destination`, and per-outpoint `destinations` overrides. Supports `dry_run`.
- `LeaveDestination` — Proto message for a single leave output target (oneof address/pk_script). Carries no amount — the server stamps the binding value at seal time (#270).
- `ROUND_STATE_QUOTE_RECEIVED = 15` — New round state emitted during the seal-time fee handshake: the client has received the server's `JoinRoundQuote` and is evaluating whether to accept or reject it.
- `NewReceiveScriptRequest` / `NewReceiveScriptResponse` — Replace `NewOORReceiveScriptRequest` / `NewOORReceiveScriptResponse`.

## Relationships

- **Depends on**: nothing (proto definitions).
- **Depended on by**: `darepod` (implements services), `cmd/darepocli/darepoclicommands` (uses generated clients), `sdk/ark` (SDK facade).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
