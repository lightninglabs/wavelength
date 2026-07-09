# RPC-over-Mailbox Contract Notes (Non-Normative)

This document captures contract notes and design constraints for an
RPC-over-mailbox transport. It is intended as a "scratchpad" companion to the
implementation-ready spec tracked in `lightninglabs/darepo`:

- Spec issue: https://github.com/lightninglabs/darepo/issues/71
- Durability reference (client-side): https://github.com/lightninglabs/darepo-client/pull/48

This document intentionally does not reference any private repositories,
internal prototypes, or local development artifacts.

## Scope

This contract describes:

- The envelope-level semantics required to safely execute request/response RPCs
  over an at-least-once mailbox transport.
- The mailbox edge API and its expected behavior around ordering, cursor/ack
  watermarks, and long-polling.
- Durability and idempotency constraints for safe retries.

This contract does not attempt to prescribe:

- The operator's internal persistence implementation.
- The client's internal scheduling model (goroutines vs actors).
- Any particular cryptographic authentication scheme.

## High-level model

The mailbox transport provides three conceptual primitives:

1. **Send**: append an envelope to a receiver's mailbox.
2. **Pull**: fetch envelopes from a mailbox, potentially long-polling.
3. **AckUpTo**: advance a receiver-side "ack watermark" so the server may
   discard older envelopes.

The RPC layer is implemented as an application-level protocol on top of the
mailbox transport:

- Requests are envelopes addressed to a `(rpc.service, rpc.method)` pair.
- Responses are envelopes correlated back to a request by a correlation id.
- Both sides MUST treat delivery as at-least-once and MUST be idempotent.

## Addressing and routing identifiers

The RPC routing keys are derived from protobuf service definitions:

- `rpc.service` is the fully-qualified protobuf service name:
  `"<proto package>.<ServiceName>"`
- `rpc.method` is the protobuf method name: `"<MethodName>"`

Example:

- `rpc.service = "arkrpc.ArkService"`
- `rpc.method = "GetInfo"`

## Idempotency and retries

The transport MUST be assumed to provide at-least-once delivery.

As a result:

- Send operations MAY be retried by the sender.
- Pull operations MAY return duplicates across retries or reconnects.
- Receivers MUST process envelopes in an idempotent manner.

The contract assumes each request envelope contains an idempotency key (a
stable identifier suitable for receiver-side deduplication). In the current
envelope model this is the envelope's dedicated `idempotency_key` field,
distinct from the `rpc.correlation_id` field used for response demuxing; the
default client implementation derives the correlation id from the
idempotency key when the caller does not override it, so the two typically
match but MUST NOT be assumed to be the same field:

- The sender MUST set an idempotency key for any request that it may retry.
- The receiver SHOULD use the idempotency key to deduplicate request handling.
- The receiver MUST NOT assume it will only see a given idempotency key once.

## Correlation

The request/response pairing is performed using a correlation identifier
carried in `rpc.correlation_id`:

- Each request has a correlation id.
- The response MUST carry the correlation id of the request it answers.
- A client with multiple in-flight requests MUST demultiplex pulled responses
  by correlation id before advancing any ack watermark.

This last rule is critical when the mailbox uses cursor-based acking: a client
that "acks too far" without demuxing can drop responses for concurrent requests.

## Ordering

Ordering constraints apply only within the scope of a single mailbox stream:

- The mailbox transport MAY provide a stable ordering for envelopes as observed
  by a single consumer.
- The RPC layer MUST NOT assume any ordering between different mailboxes.
- The RPC layer MUST NOT require strict ordering between requests and responses
  beyond correlation.

## Ack watermark

Acking is described as advancing a watermark rather than deleting individual
messages.

Expected properties:

- Acking is monotonic: an `AckUpTo` call MUST NOT decrease the watermark.
- Acking MUST be safe to retry.
- The server MAY garbage-collect envelopes strictly older than the watermark.

Clients SHOULD ack only after persisting any state required to ensure they do
not lose in-flight responses across restarts.

## Long-poll pull

Pull supports a long-poll mode to avoid tight polling loops:

- The client MAY request a long-poll timeout.
- The server SHOULD return promptly when new envelopes arrive.
- The server MAY return earlier than the full timeout.
- The server MUST return before the timeout elapses (i.e., it is a bounded
  operation).

## Payload encoding

The payload carried inside an RPC envelope is protobuf-encoded.

Implications:

- Builders do not need `protoc` to compile the repository because generated
  `*.pb.go` files are checked in.
- Contributors who regenerate protos MUST have a working `make rpc` setup.

Unknown fields:

- Receivers SHOULD unmarshal in a forward-compatible mode (discarding unknown
  fields).

## Error handling

The transport layer (mailbox) and the application layer (RPC) have distinct
failure domains:

- Transport failures include temporary unavailability, timeouts, and rate
  limiting.
- Application failures include invalid arguments, unauthorized access, and
  domain-specific errors.

The RPC envelope SHOULD provide a way to represent an application error as a
response payload, so that application errors can be correlated back to requests
without relying on transport-level error channels.

## Versioning and extensibility

The mailbox transport and RPC overlay MUST be designed for evolution:

- New payload message types will be added over time.
- New RPC services and methods will be added over time.
- Backwards-compatible extension should be preferred (e.g., adding fields).

Generated RPC-over-mailbox stubs should be treated as a convenience layer; they
MUST NOT prevent using the underlying transport for experimental or
operator-specific payloads.
