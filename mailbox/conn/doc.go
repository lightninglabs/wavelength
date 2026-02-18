// Package conn provides reusable mailbox connector primitives shared by
// client-side and server-side connector runtimes.
//
// The package intentionally contains protocol-adjacent building blocks only:
//
//   - typed identifiers and deterministic idempotency helpers,
//   - ack watermark state machine encoding for checkpoint persistence, and
//   - in-memory response waiter registry for unary correlation delivery.
//
// Higher-level runtime wiring (actor lifecycle, dispatcher tables, transport
// loops) lives in connector-specific packages such as serverconn.
package conn
