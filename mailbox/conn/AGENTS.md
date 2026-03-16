# mailbox/conn

## Purpose

Mailbox connection primitives shared by both `clientconn` and `serverconn`
for envelope identity, ack state tracking, and unary response correlation.

## Key Types

- `ResponseRegistry` — Maps correlation IDs to unary RPC waiters and early
  response buffers. `DeliverResponse` returns a tri-state result (`waiter`,
  `buffered`, `dropped`) so callers can decide whether to fall back to durable
  route dispatch for responses that arrive with no live waiter.
- `AckState` — Four-cursor watermark state machine for ingress pull/ack.
- `WrappedProto` — TLV-serializable wrapper for proto messages.
- `EnvelopeIdentity` — Stable message ID and idempotency key derivation from
  payload hash.

## Relationships

- **Depends on**: `mailbox/pb` (envelope proto), `mailbox/rpc` (ServiceMethod).
- **Depended on by**: `serverconn` (ingress, UnaryFacade), `clientconn` (ingress).

## Invariants

- Response waiters are cleaned up after TTL expiry to prevent memory leaks.
- Early-buffered responses are consumed exactly once when the waiter arrives.
- The tri-state delivery result must be checked by the ingress; `buffered` means
  the response was stored but may need durable dispatch.
