# rpc/restclient

## Purpose

HTTP/protoJSON transport adapter for grpc-gateway clients. Provides a
`Client` that speaks the grpc-gateway JSON API shape, a `StreamClient[T]`
that adapts chunked server-streaming responses to the `grpc.ServerStreamingClient`
interface, and per-service factory functions so callers are channel-agnostic
(gRPC or REST).

## Key Types

- `Client` — Shared HTTP transport for proto-JSON requests. `Post(ctx, path,
  in, out)` for unary calls; `Stream(ctx, path, in) → *http.Response` for
  server-streaming. Infers the HTTP scheme from the address (loopback →
  HTTP, non-loopback → HTTPS). Carries optional headers added via `WithHeader`.
- `Option` — Functional option (`WithHTTPClient`, `WithHeader`) applied to
  a `Client` at construction.
- `StreamClient[T]` — Adapts a chunked JSON HTTP response to
  `grpc.ServerStreamingClient[T]`. Implements `Recv()`, `Header()`,
  `Trailer()`, `CloseSend()`, `Context()`, `SendMsg()`, `RecvMsg()`.
  Skips empty chunks; aggregates error responses.
- `GatewayError` — Decoded JSON error envelope (Code, Message). `Code` may
  be an int or a string; falls back to the HTTP status code if absent.
- Per-service client types with `New*ServiceClient(addr, ...Option)` factories:
  `ArkServiceClient`, `DaemonServiceClient`, `MailboxServiceClient`,
  `SwapClientServiceClient`, `SwapServiceClient`, `WalletServiceClient`.

## Relationships

- **Depends on**: `arkrpc`, `waverpc`, `mailbox/pb`, `rpc/swapclientrpc`,
  `rpc/wavewalletrpc`, `swaprpc` (for service stub interfaces it implements).
- **Depended on by**: `sdk/wavewalletdk` (REST fallback transport),
  `sdk/swaps` (REST gRPC conn), `swapclientserver` (outbound REST
  client), `waved/outbound_clients` (outbound mailbox/swap-server
  REST clients).
- **Sends**: HTTP POST/GET requests to a grpc-gateway endpoint.
- **Receives**: chunked JSON responses or HTTP error bodies.

## Invariants

- Schemeless addresses default to HTTP for loopback (`localhost`, `::1`,
  `127.0.0.1`) and HTTPS otherwise (security-by-default for hosted deployments).
- JSON marshaling uses `UseProtoNames: true` (snake_case) for requests and
  `DiscardUnknown: true` for responses (forward compatibility).
- gRPC context metadata is injected as HTTP headers; binary-encoded
  (`-bin`-suffixed) keys are skipped.
- `WatchRounds`, `SubscribeSwaps`, and `SubscribeWallet` are the only
  server-streaming methods; all other methods are unary.
- Generated service clients implement the same gRPC-generated interfaces
  as the generated gRPC stubs, so callers do not branch on transport type.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
- [rpc/wavewalletrpc/CLAUDE.md](../wavewalletrpc/CLAUDE.md) — WalletService stubs
  this package implements over HTTP.
