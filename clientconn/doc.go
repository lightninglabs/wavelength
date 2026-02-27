// Package clientconn implements the server-side bridge for communicating
// with N clients via the durable actor runtime and mailbox system.
//
// The package mirrors the semantics of the client-side serverconn
// package, but from the server's perspective with a 1:N topology: each
// registered client gets its own bidirectional mailbox pair,
// DurableActor for crash-safe egress, ingress loop for pulling client
// messages, response registry for unary RPCs, and UnaryFacade.
//
// The top-level entry point is ClientsConnBridge, which implements
// actor.TellOnlyRef[ClientConnMsg] and routes outbound server events
// to per-client DurableActors based on ClientID. The bridge is wired
// into the rounds actor via the ClientsConn field.
//
// # Architecture
//
//	rounds.Actor
//	    │
//	    ▼ Tell(SendServerEventRequest{Message: clientMsg})
//	ClientsConnBridge
//	    │  └── Routes by ClientID to per-client ClientRuntime
//	    ▼
//	ClientRuntime (per-client)
//	    ├── DurableActor[connectorMsg, connectorResp] (egress)
//	    ├── Ingress loop (pulls from server's per-client mailbox)
//	    ├── ResponseRegistry (unary RPC waiters)
//	    └── UnaryFacade (implements mailboxrpc.RPCClient)
//
// # Async-First Model
//
// The mailbox service is the durable buffer. The server sends events to
// a client's mailbox regardless of whether the client is currently
// connected. Messages accumulate in the mailbox and are delivered when
// the client next polls. The server does not need to know if the client
// is online — Edge.Send() writes to the mailbox service, and the client
// pulls whenever it is active.
package clientconn
