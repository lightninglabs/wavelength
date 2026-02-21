# Server Connection Runtime

The `serverconn` package provides the unified connector boundary for all mailbox
traffic between the client and the remote server. It combines durable egress
(crash-safe event delivery), low-latency unary RPCs, background ingress polling,
and typed event routing into a single `Runtime` that integrates with the actor
system.

For the full three-layer architecture, see
[`docs/mailbox_architecture.md`](../docs/mailbox_architecture.md). For the
underlying mailbox primitives, see [`mailbox/README.md`](../mailbox/README.md).

## Architecture

The runtime composes three main components:

```mermaid
flowchart TB
    subgraph "Runtime"
        DA["DurableActor&lt;ServerConnMsg, ServerConnResp&gt;"]
        SCA["ServerConnectionActor"]
        UF["UnaryFacade"]
    end

    DA -->|"wraps"| SCA
    UF -->|"delegates to"| SCA

    SCA --> RR["ResponseRegistry"]
    SCA --> EDGE["Edge<br/>(MailboxServiceClient)"]

    CFG["ConnectorConfig"] -.->|"feeds"| RT["NewRuntime(cfg)"]

    ROUND["Round Actor"] -->|"Tell(SendClientEventRequest)"| DA
    CALLER["RPC Caller / Generated Stub"] -->|"SendRPC / AwaitRPC"| UF
    ER["EventRouter"] -.->|"AsDispatcherMap()"| CFG
```

- **`ServerConnectionActor`**: The core behavior. Handles egress messages in
  `Receive()` and runs the ingress loop as a background goroutine. Owns the
  in-memory `ResponseRegistry`.
- **`DurableActor`**: Wraps the actor for crash-safe egress. Persists outbound
  FSM events to a durable mailbox before processing.
- **`UnaryFacade`**: Implements `mailboxrpc.RPCClient`. Sends RPCs directly via
  the edge (low-latency, no durability) and awaits responses through the
  registry.

## Getting Started

### Creating a Runtime

```go
cfg := serverconn.DefaultConnectorConfig()
cfg.Edge = mailboxClient          // MailboxServiceClient (gRPC)
cfg.LocalMailboxID = "client-1"   // This client's mailbox
cfg.RemoteMailboxID = "server-1"  // Server's mailbox
cfg.Store = deliveryStore         // actor.DeliveryStore for persistence
cfg.Dispatchers = eventRouter.AsDispatcherMap()  // Inbound routing

runtime, err := serverconn.NewRuntime(cfg)
if err != nil {
    return err
}
```

`NewRuntime` validates required fields, creates the
`ServerConnectionActor`, wraps it in a `DurableActor` (ID:
`"serverconn-" + localMailboxID`), and creates the `UnaryFacade`. The
`Codec` field defaults to `NewServerConnCodec()` if not set.

### Starting and Stopping

```go
err := runtime.Start(ctx)
if err != nil {
    return err  // Ingress checkpoint load failed — fatal.
}
defer runtime.Stop()
```

`Start` launches the DurableActor (begins processing egress inbox) and the
ingress loop (loads ack checkpoint, starts pulling). `Stop` cancels the ingress
loop, waits for it to exit, then stops the DurableActor.

## Unary RPC: Using Generated Stubs

Generated mailbox RPC stubs call `SendRPC` + `AwaitRPC` under the hood:

```go
client := hellotestpb.NewHelloServiceMailboxClient(runtime.Unary())

resp, err := client.SayHello(ctx, &hellotestpb.HelloRequest{
    Name: "Alice",
})
if err != nil {
    // gRPC status errors are preserved through header transport.
    return err
}
```

```mermaid
sequenceDiagram
    participant C as Caller
    participant S as Generated Stub
    participant UF as UnaryFacade
    participant RR as ResponseRegistry
    participant E as Edge
    participant SRV as Server
    participant IL as Ingress Loop

    C->>S: SayHello(ctx, req)
    S->>UF: SendRPC(method, req)
    UF->>RR: RegisterWaiter(corrID)
    UF->>E: Send(KIND_REQUEST envelope)
    E->>SRV: Deliver request

    SRV->>SRV: Process
    SRV->>E: Send(KIND_RESPONSE)

    IL->>E: Pull(cursor)
    E-->>IL: [KIND_RESPONSE]
    IL->>RR: DeliverResponse(corrID, env)
    RR-->>UF: Future completes

    S->>UF: AwaitRPC(corrID, resp)
    UF-->>S: resp
    S-->>C: (resp, nil)
```

The send path calls `Edge.Send` directly — no durable mailbox, no actor queue
roundtrip. This provides low latency for unary RPCs. If the send fails, the
caller retries (no crash durability needed for unary RPCs).

Error handling: Server-side gRPC errors are encoded in envelope headers as
base64 `google.rpc.Status`. `AwaitRPC` decodes them before inspecting the body,
so callers receive standard `status.Error` values.

## Durable Event Egress: Sending FSM Events

FSM outbox messages use the durable egress path for crash safety:

```go
err := runtime.TellRef().Tell(ctx, &serverconn.SendClientEventRequest{
    Message: &joinGreetingServerMsg{SessionID: "session-1"},
})
```

The `ServerMessage` interface requires a single method:

```go
type ServerMessage interface {
    ToProto() proto.Message
}
```

The durable egress path:

1. `DurableActor.Tell` persists the `SendClientEventRequest` to the durable
   mailbox (TLV-encoded).
2. The actor runtime calls `Receive`, which:
   - Calls `Message.ToProto()` to get the proto payload.
   - Wraps it in `anypb.Any`.
   - Derives `msg_id` and `idempotency_key` from the payload SHA256 hash
     (via `StableEventMsgID` / `StableEventIdempotencyKey`).
   - Builds a `KIND_EVENT` envelope and calls `Edge.Send`.
3. On crash, the durable mailbox replays the persisted request. The same IDs are
   re-derived from the stored payload, so the server deduplicates the retry.

## Server-Push Events: Receiving and Routing

### Implementing InboundServerMessage

Actor messages that arrive from the server implement `InboundServerMessage`:

```go
type InboundServerMessage interface {
    FromProto(proto.Message) error
}
```

Combined with `actor.Message`, this forms the `InboundActorMessage` type
constraint used by `NewEventRoute`:

```go
type helloStartedMsg struct {
    actor.BaseMessage
    SessionID string
}

func (m *helloStartedMsg) MessageType() string {
    return "HelloStartedMsg"
}

func (m *helloStartedMsg) FromProto(p proto.Message) error {
    ev, ok := p.(*hellotestpb.HelloStartedEvent)
    if !ok {
        return fmt.Errorf("unexpected proto type: %T", p)
    }
    m.SessionID = ev.SessionId
    return nil
}
```

### Registering Routes with EventRouter

Create an `EventRouter`, register routes for each `(service, method)` pair, then
pass the dispatcher map to the connector config:

```go
router := serverconn.NewEventRouter(system)

// Auto-adapt route (for InboundActorMessage types):
serverconn.NewEventRoute(router, serverconn.InboundEventRouteConfig[
    *helloStartedMsg, struct{},
]{
    Service:  "hellotest.v1.HelloService",
    Method:   "HelloStarted",
    NewEvent: func() proto.Message {
        return &hellotestpb.HelloStartedEvent{}
    },
    Key:    greetingActorKey,
    NewMsg: func() *helloStartedMsg {
        return &helloStartedMsg{}
    },
})

// Manual adapt route (full control):
serverconn.AddRoute(router, serverconn.EventRouteConfig[RoundMsg, RoundResp]{
    Service:  "arkrpc.v1.RoundService",
    Method:   "RoundStarted",
    NewEvent: func() proto.Message {
        return &arkrpc.RoundStartedEvent{}
    },
    Key: roundActorKey,
    Adapt: func(p proto.Message) (RoundMsg, error) {
        return adaptRoundStarted(p)
    },
})

// Wire into connector config:
cfg.Dispatchers = router.AsDispatcherMap()
```

```mermaid
flowchart LR
    E["Edge.Pull"] --> IL["Ingress Loop"]
    IL -->|"(service, method)"| DM["Dispatchers Map"]
    DM --> DC["Dispatcher Closure"]
    DC -->|"Unmarshal body"| PROTO["proto.Message"]
    DC -->|"Adapt()"| MSG["Actor Message"]
    DC -->|"Tell()"| TA["Target Durable Actor"]
    TA --> DB[(Store)]
```

Each dispatcher closure captures a `ServiceKey`, resolves the actor via the
Receptionist, and calls `Tell` to durably persist the message. A `nil` return
means the envelope is committed — the ingress loop can safely advance the ack
watermark.

## ConnectorConfig Reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `Edge` | `MailboxServiceClient` | *required* | gRPC client for the remote mailbox edge. |
| `LocalMailboxID` | `string` | *required* | This client's mailbox identifier. |
| `RemoteMailboxID` | `string` | *required* | Remote server's mailbox identifier. |
| `ProtocolVersion` | `uint32` | `0` | Protocol version stamped on outbound envelopes. |
| `Dispatchers` | `map[ServiceMethod]EnvelopeDispatcher` | `nil` | Inbound envelope routing table. |
| `Store` | `actor.DeliveryStore` | *required* | Durability store for inbox, outbox, and checkpoints. |
| `Codec` | `*actor.MessageCodec` | `NewServerConnCodec()` | TLV codec for ServerConnMsg serialization. |
| `PullMaxEnvelopes` | `uint32` | `50` | Max envelopes per Pull call. |
| `PullWaitTimeout` | `time.Duration` | `5s` | Long-poll timeout for Pull. |
| `RetryBaseDelay` | `time.Duration` | `200ms` | Exponential backoff base for transient failures. |
| `RetryMaxDelay` | `time.Duration` | `30s` | Backoff cap. |
| `ResponseWaiterTTL` | `time.Duration` | `10m` | TTL for response waiters and buffered responses. |

Source: `serverconn/types.go`

## Crash Recovery

Two independent recovery paths operate on startup:

```mermaid
sequenceDiagram
    participant P as Process
    participant DB as Store
    participant DA as DurableActor
    participant IL as Ingress Loop
    participant E as Edge

    P->>DB: loadCheckpoint(AckState)
    P->>DA: Start() — replay egress inbox
    DA->>DB: Load persisted SendClientEventRequest(s)
    DA->>DA: Receive() — re-derive same IDs from payload hash
    DA->>E: Edge.Send (server deduplicates via idempotency key)

    P->>IL: StartIngress(ctx)
    IL->>E: AckUpTo(AckTarget) — catch up if ack was pending
    IL->>E: Pull(PullCursor) — resume from last checkpoint
    Note over IL,E: Re-pulled envelopes dispatched normally.<br/>Durable actors deduplicate via message ID.
```

**Egress recovery**: The DurableActor replays all unacknowledged
`SendClientEventRequest` and `SendRPCRequest` messages from its persistent
inbox. For event messages, the same `msg_id` and `idempotency_key` are
reproduced from the persisted TLV payload. The server deduplicates.

**Ingress recovery**: `loadCheckpoint` restores the four-cursor `AckState`.
The loop resumes from `PullCursor`. If an ack was pending at crash time
(`AckTarget > AckCommittedTo`), it acks first. Re-pulled envelopes are
dispatched normally — the target durable actors deduplicate via message ID.

**Unary RPC recovery**: Response waiters are in-memory only. On crash, callers'
contexts are cancelled and they retry the RPC with new correlation IDs.

## Message Types and TLV Encoding

Two message types flow through the durable actor mailbox:

| TLV Type | Message | Description |
|----------|---------|-------------|
| `2000` | `SendClientEventRequest` | FSM outbox event. TLV records: proto payload (Any), msg_id, idempotency_key. |
| `2001` | `SendRPCRequest` | Pre-built unary RPC envelope. TLV record: full Envelope via WrappedProto. |

Both implement `actor.TLVMessage` (`TLVType`, `Encode`, `Decode`).
`NewServerConnCodec()` returns a `MessageCodec` with both types registered.

## Testing

The package has comprehensive test coverage across several test files:

| File | Focus |
|------|-------|
| `e2e_test.go` | Full round-trip: unary RPC, server push events, durable egress, combined flows. |
| `connector_test.go` | ServerConnectionActor unit tests for egress handling. |
| `unary_facade_test.go` | UnaryFacade send/await with mocked edge. |
| `ingress_error_test.go` | Ingress loop error handling and backoff behavior. |
| `ingress_property_test.go` | Property-based tests for ack watermark invariants. |
| `actor_tlv_test.go` | TLV encode/decode round-trip for message types. |
| `restart_replay_test.go` | Crash recovery and egress replay. |
| `runtime_test.go` | Runtime lifecycle (start, stop, validation). |
| `testutil_test.go` | In-memory mailbox, checkpoint store, test helpers. |

Run tests:

```bash
# All serverconn tests:
make unit pkg=serverconn timeout=5m

# Specific test:
make unit pkg=serverconn case=TestE2E_UnaryRPC timeout=5m

# With debug logs:
make unit log="stdlog trace" pkg=serverconn case=TestE2E timeout=5m
```

## See Also

- [`docs/mailbox_architecture.md`](../docs/mailbox_architecture.md) —
  Comprehensive architecture covering all three layers with diagrams.
- [`mailbox/README.md`](../mailbox/README.md) — Mailbox module overview
  (proto definitions, RPC interfaces, connector primitives).
- [`docs/RPC_MAILBOX_CONTRACT.md`](../docs/RPC_MAILBOX_CONTRACT.md) —
  Protocol-level contract (ordering, idempotency, ack semantics).
- [`docs/durable_actor_architecture.md`](../docs/durable_actor_architecture.md)
  — Underlying actor durability model (CDC, leasing, deduplication).
- [`docs/durable_actor_quickstart.md`](../docs/durable_actor_quickstart.md) —
  Practical guide to implementing durable actors and TLV messages.
