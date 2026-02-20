// Package oor implements server-side Off-Operator-Routing transfer processing.
//
// # Durability model
//
// The coordinator actor runs on top of the shared durable mailbox runtime from
// darepo-client. Each inbox message is persisted first, then delivered to the
// actor behavior. The behavior maintains deterministic per-session FSM state
// and persists side-effect results through the outbox boundary.
//
// At a high level:
//
//  1. Durable mailbox persists inbound message.
//  2. Coordinator decodes message and drives the per-session FSM.
//  3. Outbox side effects are executed via OutboxHandler.
//  4. Session state changes are persisted through the session store.
//
// # Recovery flow
//
// On restart, the actor rebuilds in-memory session FSMs from the
// DB-authoritative session store. Active sessions (cosigned and
// awaiting_notify) are loaded and materialized as running FSM instances.
// The durable mailbox then replays any pending messages on top of this
// restored state.
//
// The runtime safety objective is:
//
//   - the same durable inbox message can be replayed without creating diverging
//     session state; and
//   - the same finalized response can be returned idempotently after restart.
//
// Human-readable flow diagram:
//
//	[Client Request]
//	      |
//	      v
//	[Durable Mailbox Persist]
//	      |
//	      v
//	[Coordinator Behavior]
//	      |
//	      v
//	[FSM Transition + Outbox Side Effects]
//	      |
//	      v
//	[Response]
//
// Restart path:
//
//	[Process Restart]
//	      |
//	      v
//	[Load Active Sessions from DB]
//	      |
//	      v
//	[Rebuild Session FSMs]
//	      |
//	      v
//	[Replay Pending Durable Messages]
package oor
