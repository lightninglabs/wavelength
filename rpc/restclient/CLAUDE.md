# rpc/restclient

## Purpose

HTTP/JSON REST client for all daemon gRPC services. Wraps the grpc-gateway
REST endpoints so host apps, SDK layers, and swap subsystems can talk to a
remote or local daemon without a gRPC dependency. Each `New*Client` function
returns a typed client that satisfies the same interface as the corresponding
generated gRPC stub.

## Key Types

- `Client` — Shared HTTP client with configurable address, headers, and
  `http.Client`. `New(addr, opts...)` constructs one.
- `Option` — Functional option for `Client`: `WithHTTPClient` overrides the
  underlying `http.Client`; `WithHeader(key, values...)` adds persistent
  request headers (e.g. auth tokens).
- `ArkServiceClient` — REST wrapper satisfying `arkrpc.ArkServiceClient`.
  Constructed by `NewArkServiceClient` / `NewArkServiceClientFromClient`.
- `DaemonServiceClient` — REST wrapper satisfying `daemonrpc.DaemonServiceClient`.
- `MailboxServiceClient` — REST wrapper satisfying `mailboxpb.MailboxServiceClient`.
- `SwapClientServiceClient` — REST wrapper satisfying `swapclientrpc.SwapClientServiceClient`.
- `SwapServiceClient` — REST wrapper satisfying `swaprpc.SwapServiceClient`.
- `WalletServiceClient` — REST wrapper satisfying `walletrpc.WalletServiceClient`.
- `StreamClient[T]` — Generic streaming-response reader wrapping an
  `*http.Response`. `NewStreamClient` wraps a server-sent-events body;
  `Recv()` reads one proto message per call.
- `GatewayError` — Typed error from the gateway with `HTTPStatus` and raw
  `Body`. `GatewayStatusError(httpStatus, body)` constructs one.

## Relationships

- **Depends on**: `arkrpc`, `daemonrpc`, `mailbox/pb`, `rpc/swapclientrpc`,
  `swaprpc`, `rpc/walletrpc` (interface types being satisfied).
- **Depended on by**: `darepod` (outbound_clients.go dials the operator ark
  service over REST), `swapclientserver` (swap server talks to the daemon
  over REST), `sdk/walletdk` (walletdk dials its embedded daemon over REST),
  `sdk/swaps` (swap FSM dials via REST).

## Invariants

- Each `New*Client(addr, opts...)` is a convenience wrapper over
  `NewArkServiceClientFromClient(New(addr, opts...))` — they share the same
  `Client` struct; hosts that need multiple service clients over the same
  connection should call `New(addr, opts...)` once and use the `FromClient`
  variants.
- `StreamClient.Recv()` returns `io.EOF` when the server closes the
  server-sent-events stream cleanly.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
