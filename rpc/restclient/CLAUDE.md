# rpc/restclient

## Purpose

Hand-written REST transport clients for all darepo gRPC services, backed by
grpc-gateway HTTP/JSON. Provides service-shaped structs (e.g.
`WalletServiceClient`, `DaemonServiceClient`) that implement the same
generated gRPC client interfaces as the native gRPC stubs, so callers can
switch between gRPC and REST transports without changing call sites.

## Key Types

- `Client` — Shared HTTP/protoJSON transport. Created with `New(addr, opts…)`.
  All per-service REST clients share one `Client` when constructed via the
  `…FromClient` constructors. Exposes `Post` (unary) and `Stream` (server
  streaming).
- `StreamClient[T]` — Adapts a grpc-gateway chunked JSON response to the
  `grpc.ServerStreamingClient[T]` interface. Note: browser/WASM runtimes may
  buffer the full response body; validate stream behavior in the target
  runtime.
- `Option` — Functional option for `Client`: `WithHTTPClient`,
  `WithHeader`.
- `GatewayError` / `GatewayStatusError` — grpc-gateway JSON error envelope
  and its converter to `google.golang.org/grpc/status` errors; preserves
  gRPC status codes across the HTTP boundary.
- Per-service clients (`ArkServiceClient`, `DaemonServiceClient`,
  `MailboxServiceClient`, `SwapClientServiceClient`, `SwapServiceClient`,
  `WalletServiceClient`) — Each implements the corresponding generated
  `…ServiceClient` interface over HTTP/JSON.

## Relationships

- **Depends on**: `arkrpc`, `daemonrpc`, `mailbox/pb`, `rpc/swapclientrpc`,
  `swaprpc`, `rpc/walletrpc` (the generated gRPC interfaces each service
  client implements), `grpc-gateway/v2/runtime`
- **Depended on by**: `sdk/walletdk` (REST transport mode via `Connect`),
  `swapclientserver` (REST dial path to swap server), `darepod` (outbound
  REST clients when `RPCTransportREST` is selected), `sdk/swaps` (swap
  server REST dial)

## Invariants

- All `New…Client(addr, opts…)` constructors create a fresh `Client`
  internally; `New…ClientFromClient(c)` constructors share the caller's
  transport, which is preferred when dialing the same host for multiple
  services.
- `StreamClient.Recv` returns `io.EOF` when the server closes the stream
  cleanly; any other error is a transport failure.
- `GatewayStatusError` preserves the numeric gRPC status code from the
  gateway JSON response so callers can rely on `status.Code(err)` as they
  would with native gRPC.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
