// Package serverconn provides the unified connector boundary for all mailbox
// traffic between the client and the remote server.
//
// The connector serves as both an egress actor and an ingress loop:
//
//   - Egress: Receives outbound messages from the round actor (FSM events) and
//     the unary facade (RPC requests), then sends them via the mailbox edge.
//     The actor is backed by a DurableActor for crash-safe egress — outbound
//     messages from the round actor persist in the durable mailbox before
//     processing, ensuring no message loss on crashes.
//
//   - Ingress: Continuously pulls envelopes from the remote mailbox, dispatches
//     them to local actors via ServiceKey-based routing, and manages the ack
//     watermark state machine to ensure at-least-once delivery with crash
//     safety.
//
// # Ack Watermark Invariants
//
// The ingress loop tracks four monotonic cursor variables in AckState:
//
//   - PullCursor: cursor for the next Pull call
//   - DispatchCommittedTo: max cursor whose envelopes have been durably
//     committed to local actor mailboxes
//   - AckTarget: max cursor that should be acked remotely (always >=
//     DispatchCommittedTo)
//   - AckCommittedTo: last cursor successfully acked to the remote edge
//
// The critical invariant is: AckUpTo only advances AFTER durable local
// dispatch commit (DurableActor.Tell returns nil = persisted). This ensures
// that if the process crashes between dispatch and ack, envelopes will be
// redelivered on restart.
//
// # Dispatch Table
//
// Inbound KIND_REQUEST and KIND_EVENT envelopes are routed via a
// map[ServiceMethod]EnvelopeDispatcher configured at wiring time. Each
// dispatcher is a closure that captures a ServiceKey reference for the target
// actor and calls Tell to durably enqueue the message.
//
// KIND_RESPONSE envelopes are delivered to in-memory response waiters via the
// response registry. This is not durable — if the process crashes, callers'
// contexts are cancelled and they retry.
//
// # Unary Facade
//
// The UnaryFacade implements mailboxrpc.RPCClient for generated RPC stubs.
// SendRPC constructs and sends envelopes directly via the mailbox edge
// (synchronous, no actor mailbox — low-latency path for unary sends).
// AwaitRPC registers a waiter in the response registry and blocks until the
// ingress loop delivers a matching KIND_RESPONSE envelope.
package serverconn
