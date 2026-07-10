# mailbox/rpc

## Purpose

Runtime interfaces used by generated RPC-over-mailbox stubs. Provides the
narrow contracts (`RPCClient`, `Router`, `HandlerFunc`) that both client stubs
and server-side routing need without including any transport implementation.

## Key Types

- `RPCClient` — Interface for generated stubs: `SendRPC` enqueues a request and
  returns a `SendResult`; `AwaitRPC` blocks until the correlated response
  arrives. Implementations must be safe for concurrent in-flight calls.
- `Router` — Interface for registering typed handlers by `(service, method)`
  pair. Consumed by the `serverconn` ingress layer and server-side generated
  code.
- `ServeMux` — Concrete in-process `Router` implementation. Maps
  `(service, method)` keys to `(newReq, HandlerFunc)` entries under a
  `sync.RWMutex`. Returns `ErrNoHandler` for unregistered routes.
- `HandlerFunc` — `func(context.Context, proto.Message) (proto.Message, error)`.
  Implementations must be idempotent (at-least-once delivery via idempotency
  key).
- `ServiceMethod` — Pairs a fully-qualified protobuf service name
  (e.g. `"arkrpc.ArkService"`) with a method name (e.g. `"GetInfo"`).
- `SendResult` — Holds the `CorrelationID` and `IdempotencyKey` returned by a
  successful `SendRPC` call. Callers pass `CorrelationID` to `AwaitRPC`.
- `RPCOptions` — Per-call overrides: `IdempotencyKey`, `CorrelationID`,
  `Headers`. All fields are optional; zero values use implementation defaults.
- `EncodeErrorHeaders` / `DecodeErrorHeaders` — Round-trip a gRPC `error` as a
  base64-encoded `google.rpc.Status` under the `HeaderGRPCStatusB64` envelope
  header, so a failed handler call can surface a typed error across the
  mailbox instead of a response body.

## Relationships

- **Depends on**: `google.golang.org/protobuf/proto` only — intentionally
  dependency-free so generated stubs can import it without pulling in transport.
- **Depended on by**: `serverconn` (wraps `RPCClient` for the durable transport
  path), `arkrpc` / `daemonrpc` (generated stubs embed the interfaces),
  `mailbox/conn` (adapts `AckState` and response registry to satisfy
  `RPCClient`).

## Invariants

- `ServeMux.Handle` panics on empty service or method strings (programming
  error, not a runtime condition).
- `HandlerFunc` implementations must be idempotent: the mailbox layer may
  redeliver the same `idempotency_key` after a crash.
- `ServiceMethod.Service` uses the fully-qualified protobuf package + service
  name, not the Go package path.
- A `KIND_RESPONSE` envelope carrying `HeaderGRPCStatusB64` signals a failed
  RPC; receivers must decode it via `DecodeErrorHeaders` before attempting to
  unmarshal the body.

## Deep Docs

- [mailbox/CLAUDE.md](../CLAUDE.md) — Parent mailbox package overview.
- [docs/mailbox_architecture.md](../../docs/mailbox_architecture.md) — Three-layer mailbox architecture.
- [docs/RPC_MAILBOX_CONTRACT.md](../../docs/RPC_MAILBOX_CONTRACT.md) — Envelope semantics and ack watermarks.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
