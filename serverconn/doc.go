// Package serverconn provides the unified connector boundary for all mailbox
// traffic between the client and the remote server.
//
// The connector serves as both an egress actor and an ingress loop:
//
//   - Egress: Receives outbound messages from round and OOR durable actors, as
//     well as unary facade calls, then sends them via the mailbox edge. The
//     actor is backed by a DurableActor for crash-safe egress: outbound actor
//     messages are durably queued before processing, so crashes do not drop
//     in-flight mailbox work.
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
// The AckState codec and related connector primitives are shared from
// mailbox/conn so the server-side connector can mirror the same behavior.
//
// # Dispatch Table
//
// Inbound KIND_REQUEST and KIND_EVENT envelopes are routed via a
// map[ServiceMethod]EnvelopeDispatcher configured at wiring time. Each
// dispatcher is a closure that captures a ServiceKey reference for the target
// actor and hands the message to it.
//
// Not every target is durable, which the dispatch contract has to account for.
// The round client and the incoming-VTXO handler are ordinary actors with
// fixed-capacity in-memory mailboxes, so a delivery to them can be refused for
// want of room. Those deliveries use TryTell and a refusal comes back as
// ErrDispatchDeferred: the cursor stops at the undelivered envelope and the
// loop re-pulls it after a backoff, which is what keeps a slow actor from
// becoming a lost event. A blocking Tell there instead would park the process's
// only mailbox puller for as long as the target took to drain, with the write
// transaction open. See deliverToActor.
//
// Durable targets refuse for a different reason: a durable mailbox has no
// in-memory capacity, but one configured with backlog watermarks turns a Tell
// away with ErrMailboxSaturated once its persistent backlog crosses the hard
// watermark. That refusal classifies as the same deferral, so a durable actor
// whose consumer has fallen ten thousand messages behind stalls the cursor
// instead of deepening its backlog — the same backpressure shape as a full
// in-memory mailbox, at a bound measured in rows instead of channel slots.
//
// Known residual: one strictly-ordered cursor feeds every inbound route, so an
// actor that stops draining still stops delivery on ALL of them. What the
// deferral changes is the blast radius and the diagnosis — the database writer
// is no longer pinned, read paths keep working, and the stall is now counted
// (ServerConnIngressDeferredTotal) and logged instead of being invisible — not
// the head-of-line coupling itself. Removing that would take a per-target
// deferral queue with its own cursor, which is a larger change than this one
// and has to preserve per-lane event_seq ordering to be worth doing.
//
// KIND_RESPONSE envelopes are delivered to in-memory response waiters via the
// response registry. This is not durable — if the process crashes, callers'
// contexts are cancelled and they retry.
//
// OOR routing is first-class in this dispatch model. Current wiring registers
// OOR service routes for:
//   - submit/finalize response adaptation into OOR DriveEvent requests;
//   - incoming transfer push events from indexer service notifications; and
//   - durable indexer-query responses used by incoming receive resolution.
//
// This means serverconn handles both outgoing OOR protocol traffic and
// the corresponding ingress callbacks needed to advance the OOR FSM after
// restart-safe delivery.
//
// Note: incoming OOR ack response routing is intentionally absent today because
// the wire proto does not yet define an ack response RPC.
//
// # Unary Facade
//
// The UnaryFacade implements mailboxrpc.RPCClient for generated RPC stubs.
// SendRPC constructs and sends envelopes directly via the mailbox edge
// (synchronous, no actor mailbox — low-latency path for unary sends).
// AwaitRPC registers a waiter in the response registry and blocks until the
// ingress loop delivers a matching KIND_RESPONSE envelope.
//
// # Runtime Composition
//
// Runtime embeds a DurableActor so it can be registered directly with the
// actor system — Ref and TellRef are promoted without wrapper methods.
// Higher layers use Runtime for round actor egress (via TellRef) and typed
// RPC stubs (via UnaryFacade).
package serverconn
