# Server-Side Dispatch Pipeline

This document is a focused reference for the mailbox RPC dispatch pipeline on
the server (arkd). It describes how client requests flow from raw mailbox
envelopes to actor FSM events, and how responses flow back.

For the underlying transport layer, see
[`clientconn_architecture.md`](clientconn_architecture.md). For FSM state
details, see [`rounds/README.md`](../rounds/README.md).

---

## End-to-End Request Path

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
│ EnvelopeDispatcher (operator makeDispatcher closure)  │
│   1. Validate envelope (non-nil Rpc, non-nil Body)   │
│   2. Inject env.Sender as ClientID into context       │
│   3. Call mux.ServeRPC(service, method, body.Value)  │
│   4. Build KIND_RESPONSE envelope with:              │
│      • CorrelationId from original request           │
│      • Error headers (if handler returned error)     │
│      • Serialized response body (if success)         │
│   5. Edge.Send(response) → client's mailbox          │
└──────────────┬───────────────────────────────────────┘
               │
               ▼
┌──────────────────────────────────────────────────────┐
│ ServeMux (mailboxrpc.ServeMux)                       │
│   • Registered handlers via generated code:          │
│     RegisterRoundServiceMailboxServer(mux, operator)  │
│     RegisterOORMailboxServiceMailboxServer(mux, op)   │
│   • Deserializes raw bytes → typed proto.Message     │
│   • Invokes typed handler method                     │
└──────────────┬───────────────────────────────────────┘
               │
               ▼
┌──────────────────────────────────────────────────────┐
│ Typed Handler (e.g. RoundOperator.JoinRound)         │
│   1. Extract ClientID from context                   │
│   2. Convert proto request → domain types            │
│      (using helpers like noncesFromProto)             │
│   3. Build actor message                             │
│   4. Send to actor via Tell() or Ask()               │
│   5. Return proto response (or error)                │
└──────────────┬───────────────────────────────────────┘
               │
               ▼
┌──────────────────────────────────────────────────────┐
│ Actor FSM (rounds.Actor, oor.Actor)                  │
│   Processes domain event, drives state transitions,  │
│   emits outbox messages                              │
└──────────────────────────────────────────────────────┘
```

---

## Wiring at Server Startup

All wiring happens during `Server.RunWithContext` in `server.go`:

### 1. Create operators

Each subsystem creates its operator during setup:

```
setupIndexerSubsystem (server_indexer.go)
  └─ indexer.NewOperator(cfg, service)
       └─ Registers on a ServeMux via RegisterIndexerServiceMailboxServer

setupRoundsSubsystem (server_rounds.go)
  └─ rounds.NewRoundOperator(cfg)
       └─ Registers on a ServeMux via RegisterRoundServiceMailboxServer

setupOORSubsystem (server_oor.go)
  └─ oor.NewOOROperator(cfg)
       └─ Registers on a ServeMux via RegisterOORMailboxServiceMailboxServer
```

### 2. Merge dispatchers on client registration

When a client connects (e.g. via the RPC server's `RegisterClient`), the
server calls `RegisterClientWithAllDispatchers` (in `server_indexer.go`):

```go
func (s *Server) RegisterClientWithAllDispatchers(ctx, clientID, baseCfg) {
    merged := make(clientconn.DispatcherMap)

    // Each returns a map of {Service, Method} → EnvelopeDispatcher
    for k, v := range s.IndexerDispatchers() { merged[k] = v }
    for k, v := range s.RoundsDispatchers()   { merged[k] = v }
    for k, v := range s.OORDispatchers()      { merged[k] = v }

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
| `ServeMux` | `mailboxrpc` | Routes `(service, method, []byte)` to typed handlers |
| `*MailboxServer` | generated | Interface with typed handler methods (e.g. `JoinRound`) |

---

## Registered Operators

| Operator | Service | Methods | Actor Target |
|----------|---------|---------|-------------|
| `RoundOperator` | `roundpb.RoundService` | `JoinRound`, `SubmitNonces`, `SubmitPartialSigs`, `SubmitForfeitSigs`, `SubmitVTXOForfeitSigs` | `rounds.Actor` via `Tell` |
| `OOROperator` | `oorpb.OORMailboxService` | `SubmitPackage`, `FinalizePackage` | `oor.Actor` via `Receive` |
| `IndexerOperator` | `arkrpc.IndexerService` | (event pub + queries) | `indexer.Service` (direct) |

---

## Response Path

After the typed handler returns, `makeDispatcher` builds a response envelope:

```go
responseEnv := &mailboxpb.Envelope{
    MsgId:     "resp-" + env.MsgId,
    Sender:    operatorSenderMailboxID,  // e.g. "svc:rounds"
    Recipient: env.Rpc.ReplyTo (or env.Sender),
    Rpc: &mailboxpb.RpcMeta{
        Kind:          KIND_RESPONSE,
        Service:       env.Rpc.Service,
        Method:        method,
        CorrelationId: env.Rpc.CorrelationId,
    },
    Headers: errorHeaders (if handler error),
    Body:    anypb.New(response) (if success),
}
```

The `CorrelationId` lets the client match this response to its original
request. Error details are encoded in envelope headers via
`mailboxrpc.EncodeErrorHeaders`.

---

## Adding a New Operator

To add a new service to the dispatch pipeline:

1. **Define the proto service** in a `.proto` file with the `mailboxrpc`
   annotation.

2. **Run `make rpc`** to generate the `*MailboxServer` interface and
   `Register*MailboxServer` function.

3. **Create an operator struct** that implements the generated interface.
   Use the existing `RoundOperator` as a template:
   - Constructor creates a `ServeMux` and calls `Register*MailboxServer`
   - `Dispatchers()` returns a `DispatcherMap` from `makeDispatcher`
   - `makeDispatcher` handles envelope validation, context injection,
     `ServeMux.ServeRPC`, response building, and `Edge.Send`

4. **Add setup method** on `Server` (e.g. `setupFooSubsystem`) that
   creates the operator.

5. **Add dispatchers to merge** in `RegisterClientWithAllDispatchers`:
   ```go
   for k, v := range s.FooDispatchers() { merged[k] = v }
   ```

---

## Tracing a Request

To trace a specific RPC through the codebase:

| Step | Where to look |
|------|--------------|
| Envelope arrives | `clientconn/ingress.go` — `dispatchEnvelope` |
| Dispatcher lookup | `clientconn/ingress.go` — `cfg.Dispatchers[key]` |
| Envelope validation | `rounds/operator.go` — `makeDispatcher` closure |
| Client ID injection | `rounds/operator.go` — `context.WithValue(ctx, clientIDContextKey{}, ...)` |
| Proto deserialization | `mailboxrpc.ServeMux.ServeRPC` |
| Handler registration | `roundpb/round_mailboxrpc.pb.go` — `RegisterRoundServiceMailboxServer` |
| Typed handler | `rounds/operator.go` — e.g. `SubmitNonces` |
| Proto→domain conversion | `rounds/operator.go` — e.g. `noncesFromProto` |
| Actor dispatch | `rounds/operator.go` — `o.cfg.RoundsRef.Tell(ctx, msg)` |
| Response envelope | `rounds/operator.go` — `makeDispatcher` closure (bottom half) |
| Response send | `rounds/operator.go` — `o.cfg.Edge.Send(ctx, ...)` |

For OOR requests, substitute `oor/operator.go` and the OOR actor.
For indexer requests, substitute `indexer/operator.go` and the indexer service.
