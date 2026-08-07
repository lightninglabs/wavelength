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
- `Retry` / `RetryWithKey` — Run one *logical* RPC under a `RetryPolicy`,
  handing every attempt the same `RPCOptions.IdempotencyKey`. The key is
  minted once at the logical-call boundary (random, not a payload digest, so
  two identical concurrent reads stay distinct). Only `ResourceExhausted` is
  retried, after a jittered exponential backoff; a cancelled or expired
  context never re-sends.
- `RetryPolicy` — `MaxAttempts` / `BaseDelay` / `MaxDelay`; the zero value
  selects the package defaults (4 attempts, 200 ms base, 5 s cap), which
  mirror `serverconn/mailboxpull`'s backoff shape.
- `NewIdempotencyKey` — Mints one 16-byte hex key per logical request.
- `IsShedError` — Reports whether an error (wrapped or not) is the operator's
  explicit `ResourceExhausted` shed answer.
- `EncodeErrorHeaders` / `DecodeErrorHeaders` — Round-trip a gRPC `error` as a
  base64-encoded `google.rpc.Status` under the `HeaderGRPCStatusB64` envelope
  header, so a failed handler call can surface a typed error across the
  mailbox instead of a response body.

## Relationships

- **Depends on**: `google.golang.org/protobuf/proto` only — intentionally
  dependency-free so generated stubs can import it without pulling in transport.
- **Depended on by**: `serverconn` (wraps `RPCClient` for the durable transport
  path), `arkrpc` / `waverpc` (generated stubs embed the interfaces),
  `mailbox/conn` (adapts `AckState` and response registry to satisfy
  `RPCClient`).

## Invariants

- `ServeMux.Handle` panics on empty service or method strings (programming
  error, not a runtime condition).
- `HandlerFunc` implementations must be idempotent: the mailbox layer may
  redeliver the same `idempotency_key` after a crash.
- `ServiceMethod.Service` uses the fully-qualified protobuf package + service
  name, not the Go package path.
- An idempotency key identifies one *logical* request, not one send attempt
  and not one request payload. Callers that re-issue must route through
  `Retry`/`RetryWithKey` (or thread an explicit `RPCOptions.IdempotencyKey`);
  leaving the field empty makes the transport mint a fresh key per attempt,
  which defeats operator-side deduplication.
- A `KIND_RESPONSE` envelope carrying `HeaderGRPCStatusB64` signals a failed
  RPC; receivers must decode it via `DecodeErrorHeaders` before attempting to
  unmarshal the body.

## Deep Docs

- [mailbox/CLAUDE.md](../CLAUDE.md) — Parent mailbox package overview.
- [docs/mailbox_architecture.md](../../docs/mailbox_architecture.md) — Three-layer mailbox architecture.
- [docs/RPC_MAILBOX_CONTRACT.md](../../docs/RPC_MAILBOX_CONTRACT.md) — Envelope semantics and ack watermarks.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
