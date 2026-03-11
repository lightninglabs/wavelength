# Server-Side Dispatch Pipeline

This document is a focused reference for the mailbox RPC dispatch pipeline on
the server (arkd). It describes how client requests flow from raw mailbox
envelopes to actor FSM events.

For the underlying transport layer, see
[`clientconn_architecture.md`](clientconn_architecture.md). For FSM state
details, see [`rounds/README.md`](../rounds/README.md).

---

## Dispatch Models

The server uses two dispatch models depending on the RPC semantics:

### Fire-and-Forget (EventRouter)

Used by **rounds** and **OOR** RPCs. The client sends a request; the server
durably commits the message to the actor's mailbox and returns nil. Responses
arrive asynchronously via the outbox event path (bridge → per-client
DurableActor → client mailbox). No response envelope is built.

```
Client sends KIND_REQUEST envelope to server's per-client mailbox
    │
    ▼
┌──────────────────────────────────────────────────────┐
│ clientconn Ingress Loop (clientconn/ingress.go)      │
│   • Pull envelopes from mailbox                      │
│   • For each envelope:                               │
│     1. Extract {Service, Method} from env.Rpc        │
│     2. Lookup DispatcherMap[{Service, Method}]        │
│     3. Call dispatcher(ctx, envelope)                 │
│   • Advance cursor + ack                             │
└──────────────┬───────────────────────────────────────┘
               │
               ▼
┌──────────────────────────────────────────────────────┐
│ EnvelopeDispatcher (from AddEnvelopeRoute)            │
│   1. Validate envelope (non-nil Body)                │
│   2. Unmarshal env.Body.Value → typed proto.Message   │
│   3. Call Adapt(env, proto) → actor message           │
│      • Extract ClientID from env.Sender              │
│      • Convert proto → domain types                  │
│   4. actorKey.Ref(system).Tell(ctx, actorMsg)        │
│      (durable commit to actor mailbox)               │
└──────────────┬───────────────────────────────────────┘
               │
               ▼
┌──────────────────────────────────────────────────────┐
│ Actor FSM (rounds.Actor, oor.Actor)                  │
│   Processes domain event, drives state transitions,  │
│   emits outbox messages → client via bridge          │
└──────────────────────────────────────────────────────┘
```

### Synchronous Request-Response (Operator)

Used by the **indexer** subsystem. The operator dispatches through a ServeMux,
builds a `KIND_RESPONSE` envelope, and sends it back via `Edge.Send`.

```
EnvelopeDispatcher (operator makeDispatcher closure)
   1. Validate envelope (non-nil Rpc, non-nil Body)
   2. Inject env.Sender as ClientID into context
   3. Call mux.ServeRPC(service, method, body.Value)
   4. Build KIND_RESPONSE envelope with:
      • CorrelationId from original request
      • Error headers (if handler returned error)
      • Serialized response body (if success)
   5. Edge.Send(response) → client's mailbox
```

---

## Wiring at Server Startup

All wiring happens during `Server.RunWithContext` in `server.go`:

### 1. Create EventRouter and operators

After the actor system is initialized, the server creates a shared
`clientconn.EventRouter`. During each subsystem setup, routes are registered:

```
Actor system initialized
  └─ s.eventRouter = clientconn.NewEventRouter(s.actorSystem)

setupIndexerSubsystem (server_indexer.go)
  └─ indexer.NewOperator(cfg, service)
       └─ Registers on a ServeMux via RegisterIndexerServiceMailboxServer

setupRoundsSubsystem (server_rounds.go)
  └─ s.registerRoundRoutes(roundsKey)
       └─ 5× clientconn.AddEnvelopeRoute (one per round RPC)

setupOORSubsystem (server_oor.go)
  └─ s.registerOORRoutes(oorKey)
       └─ 2× clientconn.AddEnvelopeRoute (SubmitPackage, FinalizePackage)
```

### 2. Merge dispatchers on client registration

When a client connects, `RegisterClientWithAllDispatchers` merges dispatchers
from two sources:

```go
func (s *Server) RegisterClientWithAllDispatchers(ctx, clientID, baseCfg) {
    merged := make(clientconn.DispatcherMap)

    // Indexer: synchronous request-response via operator
    for k, v := range s.IndexerDispatchers() { merged[k] = v }

    // Rounds + OOR: fire-and-forget via EventRouter
    for k, v := range s.eventRouter.AsDispatcherMap() { merged[k] = v }

    baseCfg.Dispatchers = merged
    return s.clientBridge.RegisterClient(ctx, clientID, baseCfg)
}
```

### 3. Ingress loop uses DispatcherMap

The `clientconn.ClientRuntime` starts an ingress loop that pulls envelopes
from the client's mailbox. For each `KIND_REQUEST` envelope, it looks up the
dispatcher by `{env.Rpc.Service, env.Rpc.Method}` and calls it.

---

## Key Types

| Type | Package | Description |
|------|---------|-------------|
| `DispatcherMap` | `clientconn` | `map[ServiceMethod]EnvelopeDispatcher` |
| `ServiceMethod` | `mailboxrpc` | `struct{Service, Method string}` — routing key |
| `EnvelopeDispatcher` | `clientconn` | `func(ctx, *Envelope) error` — dispatch closure |
| `EventRouter` | `clientconn` | Collects `AddEnvelopeRoute` registrations, returns `DispatcherMap` |
| `EnvelopeRouteConfig` | `clientconn` | Typed config for fire-and-forget envelope routes |
| `ServeMux` | `mailboxrpc` | Routes `(service, method, []byte)` to typed handlers (indexer only) |

---

## Registered Routes

| Dispatch Model | Service | Methods | Actor Target |
|----------------|---------|---------|-------------|
| `AddEnvelopeRoute` | `round.v1.RoundService` | `JoinRound`, `SubmitNonces`, `SubmitPartialSigs`, `SubmitForfeitSigs`, `SubmitVTXOForfeitSigs` | `rounds.Actor` via `Tell` |
| `AddEnvelopeRoute` | `oorpb.OORMailboxService` | `SubmitPackage`, `FinalizePackage` | `oor.Actor` via `Tell` |
| Operator (ServeMux) | `arkrpc.IndexerService` | (event pub + queries) | `indexer.Service` (direct) |

---

## Adding a New Fire-and-Forget Route

To add a new fire-and-forget service to the dispatch pipeline:

1. **Define the proto service** in a `.proto` file and run `make rpc`.

2. **Define actor message type** implementing `actor.Message` in the target
   actor's message file.

3. **Register the route** in a setup method using `AddEnvelopeRoute`:
   ```go
   clientconn.AddEnvelopeRoute(
       s.eventRouter,
       clientconn.EnvelopeRouteConfig[MyActorMsg, MyActorResp]{
           Service:  "my.v1.MyService",
           Method:   "DoSomething",
           NewEvent: func() proto.Message { return &mypb.DoSomethingRequest{} },
           Key:      myActorKey,
           Adapt: func(env *mailboxpb.Envelope,
               p proto.Message) (MyActorMsg, error) {
               req := p.(*mypb.DoSomethingRequest)
               // Convert proto → domain, extract env.Sender if needed
               return &MyActorCommand{...}, nil
           },
       },
   )
   ```

4. **No additional merge step needed** — the EventRouter's `AsDispatcherMap`
   is already merged in `RegisterClientWithAllDispatchers`.

For synchronous request-response RPCs, use the operator pattern (see
`indexer/operator.go` for the template).

---

## Tracing a Request

### Fire-and-forget (Rounds/OOR)

| Step | Where to look |
|------|--------------|
| Envelope arrives | `clientconn/ingress.go` — `dispatchEnvelope` |
| Dispatcher lookup | `clientconn/ingress.go` — `cfg.Dispatchers[key]` |
| Proto deserialization | `clientconn/event_router.go` — `AddEnvelopeRoute` closure |
| ClientID extraction | `server_rounds.go` — `Adapt` closure: `clientconn.ClientID(env.Sender)` |
| Proto→domain conversion | `rounds/proto_convert.go` — e.g. `NoncesFromProto` |
| Actor dispatch | `clientconn/event_router.go` — `actorKey.Ref(system).Tell(ctx, actorMsg)` |

### Synchronous (Indexer)

| Step | Where to look |
|------|--------------|
| Envelope validation | `indexer/operator.go` — `makeDispatcher` closure |
| Client ID injection | `indexer/operator.go` — `context.WithValue(ctx, ...)` |
| Proto deserialization | `mailboxrpc.ServeMux.ServeRPC` |
| Typed handler | `indexer/operator.go` handler methods |
| Response envelope | `indexer/operator.go` — `makeDispatcher` closure (bottom half) |
| Response send | `indexer/operator.go` — `o.cfg.Edge.Send(ctx, ...)` |
