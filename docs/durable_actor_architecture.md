# Durable Actor Architecture

This document explains the key concepts and patterns in the durable actor
system. It covers the CDC (Change Data Capture) pattern, message delivery
semantics, recovery mechanisms, and type-erased actor discovery.

## Table of Contents

1. [Overview](#overview)
2. [OutboxPublisher CDC Pattern](#outboxpublisher-cdc-pattern)
3. [DurableMailbox Message Lifecycle](#durablemailbox-message-lifecycle)
4. [Actor System Architecture](#actor-system-architecture)
5. [Lease-Based Delivery Semantics](#lease-based-delivery-semantics)
6. [Recovery and Restart Flow](#recovery-and-restart-flow)
7. [TypeAssertingRef and MapRef Pattern](#typeassertingref-and-mapref-pattern)
8. [DurableAsk: Crash-Safe Request-Response](#durableask-crash-safe-request-response)

---

## Overview

The durable actor system provides crash-resilient message processing for actors.
It combines several patterns to ensure no message loss and exactly-once
processing semantics.

The core insight is that crashes can happen at any point: after receiving a
message but before processing, after processing but before sending a response,
or after sending but before acknowledging. Each pattern addresses a specific
failure mode:

- **Inbox Durability**: Messages are persisted before delivery. If the actor
  crashes before processing, the message survives and will be redelivered.

- **Transactional Outbox (CDC)**: Outgoing messages are written atomically with
  FSM state changes. This prevents the "state updated but message lost" problem
  when a crash occurs between state update and message send.

- **Lease-Based Delivery**: Prevents stale acknowledgments and enables automatic
  redelivery. A crashed consumer's lease expires, making the message available
  to another consumer (or the same consumer after restart).

- **Deduplication**: Tracks processed message IDs with TTL. When a message is
  redelivered, the actor checks if it was already processed and skips re-execution.
  This turns at-least-once delivery into exactly-once processing.

- **Checkpointing**: Persists FSM state after each message. On restart, the actor
  loads its checkpoint and continues from where it left off rather than starting
  from scratch.

```mermaid
flowchart TB
    subgraph "Durable Actor System"
        subgraph "Inbox Path"
            S[Sender] -->|Tell/Ask| DM[DurableMailbox]
            DM -->|Persist| MM[(mailbox_messages)]
            MM -->|Lease| DA[DurableActor]
        end

        subgraph "Outbox Path"
            DA -->|Write in TX| OM[(outbox_messages)]
            OM -->|Poll| OP[OutboxPublisher]
            OP -->|Deliver| TM[Target Mailbox]
        end

        subgraph "State Management"
            DA -->|Checkpoint| CP[(fsm_checkpoints)]
            DA -->|Deduplicate| PM[(processed_messages)]
            DA -->|Dead Letter| DL[(dead_letters)]
        end
    end
```

---

## OutboxPublisher CDC Pattern

The OutboxPublisher implements the Change Data Capture (CDC) pattern for
reliable inter-actor messaging. When an actor needs to send a message to another
actor, it writes to the outbox table within the same transaction as its FSM
state changes. This ensures atomicity: either both the state change and the
outbox message persist, or neither does.

### CDC Sequence Diagram

```mermaid
sequenceDiagram
    participant A as Actor A
    participant TX as Transaction
    participant FSM as fsm_checkpoints
    participant OB as outbox_messages
    participant P as OutboxPublisher
    participant R as Receptionist
    participant B as Actor B Mailbox

    Note over A,TX: Begin Transaction
    A->>TX: Begin
    A->>FSM: SaveCheckpoint(new_state)
    A->>OB: EnqueueOutbox(message)
    A->>TX: Commit
    Note over A,TX: Transaction Complete

    loop Poll Interval (1s)
        P->>OB: ClaimOutboxBatch()
        OB-->>P: Pending messages
    end

    P->>P: Decode with MessageCodec
    P->>R: ServiceKey[Message, any].Ref(target_id)
    R-->>P: ActorRef[Message, any]
    P->>B: Tell(decoded_message)

    alt Delivery Success
        P->>OB: CompleteOutbox(id)
    else Delivery Failure
        Note over P: Leave for retry
        P->>P: Next poll will retry
    end
```

### Why CDC?

Without CDC, there's a window where an actor could:
1. Update its FSM state
2. Crash before sending the outgoing message
3. Restart and have inconsistent state (state updated but message never sent)

With CDC, the outbox write is part of the same transaction as the state update.
If the transaction commits, the message is guaranteed to be delivered
(eventually). If it rolls back, neither happens.

### OutboxPublisher Configuration

```go
type OutboxPublisherConfig struct {
    Store               DeliveryStore // Persistence layer
    Codec               *MessageCodec // Message serialization
    System              SystemContext // Actor discovery via ServiceKey
    PollInterval        time.Duration // Default: 1s (fallback; commits wake immediately)
    BatchSize           int           // Default: 100
    MaxDeliveryAttempts int           // Default: 10
    ClaimDuration       time.Duration // Default: 30s
}
```

### Message Flow

```mermaid
flowchart LR
    subgraph "Actor Transaction"
        B[Behavior.Receive] --> C{Success?}
        C -->|Yes| D[SaveCheckpoint]
        D --> E[EnqueueOutbox]
        E --> F[Ack Message]
        F --> G[Commit TX]
    end

    subgraph "OutboxPublisher"
        G --> H[ClaimOutboxBatch]
        H --> I[Decode Message]
        I --> J[Lookup ServiceKey]
        J --> K[Tell Target]
        K --> L{Success?}
        L -->|Yes| M[CompleteOutbox]
        L -->|No| N[Retry on next poll]
    end
```

---

## DurableMailbox Message Lifecycle

Messages in the DurableMailbox follow a specific lifecycle from enqueue to
acknowledgment. The lifecycle ensures at-least-once delivery with exactly-once
processing.

### Message State Machine

```mermaid
stateDiagram-v2
    [*] --> Enqueued: Send()
    Enqueued --> Available: available_at reached
    Available --> Leased: LeaseNextMessage()
    Leased --> Acked: Ack()
    Leased --> Available: Nack() or Lease Expired
    Leased --> DeadLettered: Max attempts exceeded
    Acked --> [*]
    DeadLettered --> [*]

    note right of Leased
        lease_token must match
        for Ack/Nack to succeed
    end note
```

### Detailed Lifecycle Flow

```mermaid
sequenceDiagram
    participant S as Sender
    participant M as DurableMailbox
    participant DB as mailbox_messages
    participant C as Consumer (DurableActor)
    participant DL as dead_letters

    S->>M: Send(envelope)
    M->>M: Encode with MessageCodec
    M->>DB: EnqueueMessage(id, payload, available_at=now)
    M-->>S: true (success)

    Note over M: Wake signal sent to consumer

    loop Processing Loop
        C->>DB: LeaseNextMessage(mailbox_id, token, duration)
        DB-->>C: LeasedMessage or nil
    end

    alt Message Available
        C->>C: Decode message
        C->>C: Check IsProcessed(id)

        alt Already Processed (Duplicate)
            C->>DB: Ack(id, token)
        else Not Processed
            C->>C: Execute Behavior.Receive()

            alt Success
                C->>DB: MarkProcessed(id)
                C->>DB: Ack(id, token)
            else Failure (Tell)
                alt Retry Policy Says Retry
                    C->>DB: Nack(id, token, delay)
                else Max Attempts Exceeded
                    C->>DL: MoveToDeadLetter(id, reason)
                    C->>DB: DeleteMessage(id)
                end
            else Failure (Ask)
                C->>DB: SaveAskResult(error)
                C->>DB: Ack(id, token)
            end
        end
    end
```

### Key Lifecycle Points

Each message passes through these states. Understanding the transitions helps
debug delivery issues and design retry strategies.

1. **Enqueue**: Message serialized via `MessageCodec` and persisted with an
   `available_at` timestamp. For immediate delivery, this is set to now. For
   delayed/scheduled messages, it's set to a future time.

2. **Available**: Message becomes eligible for delivery when `available_at <= now`.
   The `LeaseNextMessage` query filters by this timestamp.

3. **Leased**: Consumer atomically claims the message by setting `lease_token`
   (a unique ID) and `lease_until` (expiry time). The token proves ownership.

4. **Processing**: Consumer executes `Behavior.Receive()`. For long operations,
   the runtime automatically extends the lease via heartbeat (every `LeaseDuration/3`).

5. **Ack**: On success, the message is deleted and its ID recorded in
   `processed_messages` for deduplication. The lease token must match.

6. **Nack**: On transient failure, the message is released by clearing
   `lease_token` and setting `available_at` to `now + retryDelay`. The message
   will be redelivered after the delay.

7. **Dead Letter**: After `max_attempts` failures, the message is moved to
   `dead_letters` with the failure reason. Dead letters require manual inspection
   and can be replayed or deleted.

---

## Actor System Architecture

The durable actor system consists of several interconnected components organized
in layers. Each layer has a specific responsibility and depends only on layers
below it.

**Actor Layer**: Your code lives here. `DurableActor` manages the lifecycle and
delegates message handling to your `ActorBehavior` implementation.

**Mailbox Layer**: `DurableMailbox` provides the message queue abstraction. It
handles serialization via `MessageCodec` and yields `Delivery` objects that wrap
messages with lease operations.

**Persistence Layer**: `DeliveryStore` is the interface; `actordelivery.Store`
is the SQLite implementation. For transactional FSM updates, use
`TxAwareActorDeliveryStore` which wraps message processing in a database
transaction.

**CDC Layer**: `OutboxPublisher` runs as a background service, polling the outbox
table and delivering messages to target actors via the Discovery Layer.

**Discovery Layer**: Actors register with the `Receptionist` using `ServiceKey`.
The `OutboxPublisher` uses `TypeAssertingRef` to bridge between type-erased
lookups and concrete actor types.

### Component Diagram

```mermaid
flowchart TB
    subgraph "Actor Layer"
        DA[DurableActor]
        AB[ActorBehavior]
        DA -->|delegates to| AB
    end

    subgraph "Mailbox Layer"
        DM[DurableMailbox]
        DA -->|owns| DM
    end

    subgraph "Delivery Layer"
        DEL[Delivery]
        DM -->|yields| DEL
        DA -->|processes| DEL
    end

    subgraph "Persistence Layer"
        DS[DeliveryStore]
        ADS[actordelivery.Store]
        TADS[TxAwareActorDeliveryStore]

        DS -.->|interface| ADS
        ADS -->|extends| TADS
        DM -->|uses| DS
        DA -->|uses| DS
    end

    subgraph "CDC Layer"
        OP[OutboxPublisher]
        MC[MessageCodec]
        OP -->|uses| DS
        OP -->|uses| MC
        DM -->|uses| MC
    end

    subgraph "Discovery Layer"
        REC[Receptionist]
        SK[ServiceKey]
        TAR[TypeAssertingRef]
        MR[MapRef]

        SK -->|lookups via| REC
        OP -->|discovers via| SK
        TAR -.->|adapter| MR
    end

    subgraph "Storage Layer"
        MM[(mailbox_messages)]
        OM[(outbox_messages)]
        PM[(processed_messages)]
        CP[(fsm_checkpoints)]
        DL[(dead_letters)]
        AR[(ask_results)]

        ADS -->|queries| MM
        ADS -->|queries| OM
        ADS -->|queries| PM
        ADS -->|queries| CP
        ADS -->|queries| DL
        ADS -->|queries| AR
    end
```

### Component Responsibilities

| Component | Responsibility |
|-----------|----------------|
| `DurableActor` | Lifecycle management, message processing loop, deduplication, automatic ack/nack |
| `DurableMailbox` | Message queue interface, lease-based iteration, priority ordering |
| `Delivery` | Message wrapper with lease operations (Ack/Nack/Extend) |
| `DeliveryStore` | Persistence interface for all mailbox operations |
| `actordelivery.Store` | SQLite implementation of DeliveryStore |
| `TxAwareActorDeliveryStore` | Adds transaction support for atomic FSM updates |
| `OutboxPublisher` | Background service draining outbox, delivering to targets |
| `MessageCodec` | TLV serialization/deserialization with type dispatch |
| `ServiceKey` | Type-safe actor discovery via Receptionist pattern |
| `TypeAssertingRef` | Adapter for type-erased actor lookups |

### DeliveryStore vs TxAwareDeliveryStore

The persistence layer offers two variants:

**DeliveryStore** (interface): Basic persistence operations. Each method runs in
its own transaction. Use this when you don't need atomic FSM updates.

**TxAwareDeliveryStore** (interface): Extends `DeliveryStore` with `ExecTx()`,
which wraps multiple operations in a single database transaction. Use this when
you need atomicity between:
- Updating FSM checkpoint
- Writing outbox messages
- Marking messages as processed
- Acknowledging the input message

When you pass a `TxAwareActorDeliveryStore` to `DurableActor`, the runtime
automatically wraps message processing in a transaction. Your `Receive()` method
can access the transaction-scoped store via context if needed.

```go
// Without TxAware: Each operation is separate transaction (no atomicity)
store.SaveCheckpoint(...)   // TX 1
store.EnqueueOutbox(...)    // TX 2 - if crash here, checkpoint saved but message lost

// With TxAware: All operations in same transaction
store.ExecTx(ctx, false, func(txCtx context.Context, txStore DeliveryStore) error {
    txStore.SaveCheckpoint(...)   // Same TX
    txStore.EnqueueOutbox(...)    // Same TX
    return nil                     // Commit or rollback together
})
```

---

## Lease-Based Delivery Semantics

Lease-based delivery prevents message loss and duplicate processing in the face
of consumer crashes. The key insight is that a consumer must prove it still
holds the lease when acknowledging a message.

### The Stale-Ack Problem

Without lease tokens, this race condition can occur:

```mermaid
sequenceDiagram
    participant C1 as Consumer 1
    participant DB as Database
    participant C2 as Consumer 2

    C1->>DB: Lease message (no token)
    Note over C1: Starts processing
    Note over C1: Processing takes too long
    Note over DB: Lease expires
    C2->>DB: Lease same message
    C2->>C2: Process message
    C2->>DB: Ack message
    Note over C1: Finishes processing
    C1->>DB: Ack message (stale!)
    Note over DB: Double-ack or error!
```

### Lease Token Solution

```mermaid
sequenceDiagram
    participant C1 as Consumer 1
    participant DB as Database
    participant C2 as Consumer 2

    C1->>DB: Lease message, get token=ABC
    Note over C1: Starts processing
    Note over C1: Processing takes too long
    Note over DB: Lease expires, token cleared
    C2->>DB: Lease same message, get token=XYZ
    C2->>C2: Process message
    C2->>DB: Ack(token=XYZ) - Success
    Note over C1: Finishes processing
    C1->>DB: Ack(token=ABC) - FAILS (token mismatch)
    Note over C1: Knows it lost the lease
```

### Lease Operations

The `Delivery` type wraps a leased message with operations to signal completion.
All operations validate the lease token before executing.

```go
type Delivery[M TLVMessage, R any] struct {
    ID         string
    Message    M
    LeaseToken string
    LeaseUntil time.Time
    Attempts   int
    // ...
}

// Ack deletes message if lease token matches
func (d *Delivery) Ack(ctx context.Context, result fn.Result[R]) error

// Nack releases message for redelivery after delay
func (d *Delivery) Nack(ctx context.Context, err error, retryAfter time.Duration) error

// Extend prolongs the lease for long-running operations
func (d *Delivery) Extend(ctx context.Context, extension time.Duration) error
```

**Automatic Heartbeat**: The `DurableActor` runtime automatically extends leases
during message processing. A background goroutine calls `Extend()` every
`LeaseDuration/3` (default: 10s when lease is 30s). This means you don't need to
manually extend leases for long operations - the runtime handles it.

If the heartbeat fails (e.g., database unavailable), processing continues but
the actor logs a warning. The message may be redelivered if the lease expires
before `Ack()` is called.

### Lease Timeline

```mermaid
gantt
    title Message Lease Timeline
    dateFormat X
    axisFormat %s

    section Consumer
    Lease Acquired    :done, 0, 1
    Processing        :active, 1, 5
    Extend Lease      :milestone, 3, 3
    Ack Message       :done, 5, 6

    section Lease Window
    Initial Lease (30s)     :0, 3
    Extended Lease (30s)    :3, 6
```

---

## Recovery and Restart Flow

When an actor restarts after a crash, it must restore its state and resume
processing. The durable actor system supports this through checkpointing and
RestartMessage priority.

### Recovery Sequence

```mermaid
sequenceDiagram
    participant A as Actor (restarting)
    participant CP as fsm_checkpoints
    participant MB as mailbox_messages
    participant B as Behavior

    Note over A: Actor Starting
    A->>CP: LoadCheckpoint(actor_id)
    CP-->>A: Checkpoint{state_type, state_data, version}

    alt Checkpoint Exists
        A->>A: Decode state_data
        A->>A: Create RestartMessage(checkpoint)
        A->>MB: Enqueue RestartMessage (priority=MAX)
        Note over MB: RestartMessage at front of queue
    end

    A->>A: Start processing loop

    loop Message Processing
        A->>MB: LeaseNextMessage()
        MB-->>A: Message (RestartMessage first due to priority)

        alt Is RestartMessage
            A->>B: Receive(RestartMessage)
            B->>B: Restore FSM state
        else Regular Message
            A->>B: Receive(message)
        end
    end
```

### RestartMessage Priority

RestartMessages have `priority=math.MaxInt32` (2147483647) to ensure they're
processed before any regular messages (which default to priority=0). This is
critical because:

1. Regular messages may depend on the restored FSM state
2. Processing regular messages before state restoration could cause errors
3. The actor needs to "catch up" to its pre-crash state first

The high priority value means RestartMessage always sorts to the front of the
queue, regardless of when other messages were enqueued.

### Recovery Flow Diagram

```mermaid
flowchart TD
    A[Actor Start] --> B{Checkpoint exists?}
    B -->|Yes| C[Load checkpoint]
    C --> D[Decode state_data]
    D --> E[Create RestartMessage]
    E --> F[Enqueue with priority=MAX]
    F --> G[Start processing loop]
    B -->|No| G

    G --> H[LeaseNextMessage]
    H --> I{Is RestartMessage?}
    I -->|Yes| J[Restore FSM state]
    J --> K[Process as normal message]
    K --> H
    I -->|No| L[Execute Behavior.Receive]
    L --> M[Update checkpoint]
    M --> H
```

### Unprocessed Messages on Restart

Messages that were leased but not acknowledged before crash are automatically
redelivered. The timing depends on when the lease expires:

1. **Lease Expiry**: `LeaseNextMessage()` treats a row as eligible once
   `lease_until < now`, so an expired lease is reclaimed atomically by the
   same query that claims the next message - no separate clearing step is
   required for redelivery to proceed. `ExpireLeases()` is available as a
   standalone maintenance operation (used in tests/tooling) that explicitly
   clears `lease_token`/`lease_until` for stale rows.

2. **Redelivery**: The restarted actor's `LeaseNextMessage()` poll picks up the
   message once its `lease_until` has passed. The `attempts` counter is
   preserved, so the message won't be retried forever if it keeps failing.

3. **Deduplication**: Before executing `Receive()`, the actor checks
   `IsProcessed(message_id)`. If the message was processed before crash (but ack
   was lost), it's skipped and immediately acked.

**Default Lease Duration**: 30 seconds. If an actor crashes, its leased messages
become available for redelivery after at most 30 seconds.

```mermaid
flowchart LR
    subgraph "Pre-Crash"
        A[Lease Message] --> B[Start Processing]
        B --> C[Crash!]
    end

    subgraph "Recovery"
        D[lease_until passes] --> E[Message available again]
        E --> F[Actor restarts]
        F --> G[Lease same message]
        G --> H{IsProcessed?}
        H -->|Yes| I[Skip, Ack]
        H -->|No| J[Process normally]
    end

    C -.->|Time passes| D
```

### In-Process Supervision: Panic-Driven Restart

A crash is not the only way an actor's in-memory state goes bad. A behavior
that panics mid-turn leaves whatever it was mutating half-updated, and the
runtime used to recover the panic, nack the message, and feed the next one to
that same, now-suspect behavior instance. The supervision kernel closes that
gap by turning a panic into the in-process equivalent of the crash recovery
above.

The runtime distinguishes the two failure shapes. A behavior that *returns* an
error is an ordinary message failure and retries per the `TellRetryPolicy`. A
behavior that *panics* is converted into a `behaviorPanic`, which the worker
recognises and hands to the actor's supervision loop.

The restart sequence is:

1. The delivery's normal ack/nack/dead-letter bookkeeping runs first, so the
   message that triggered the panic burns an attempt exactly as it does today.
   This ordering is what keeps a deterministic poison message climbing toward
   `max_attempts` and the dead letter queue instead of restarting the actor
   forever. On the classic path, where the whole `Receive` runs inside one
   framework transaction, that bookkeeping is deliberately redone OUTSIDE the
   transaction: supervision returns the panic from the transaction to force a
   rollback of the behavior's own partial writes, because committing them
   alongside the nack would persist the torn state the restart exists to escape
   and the checkpoint reload would hand it straight back.
2. Supervision cancels the current worker *generation*, which drains every
   worker of a `NumWorkers > 1` pool, not just the one that panicked. Several
   workers panicking together cost one restart between them, not one each.
3. The behavior's `OnStop` hook runs (bounded by `CleanupTimeout`) if it
   implements `Stoppable`. A panic escaping that hook is recovered rather than
   allowed to take the process down, and terminates the actor.
4. The startup path re-runs: `LoadCheckpoint` followed by
   `PrependRestartMessageWithID`, so the behavior can rebuild from its
   persisted FSM state before it sees any other message.
5. A fresh generation of `NumWorkers` loops starts on the same mailbox, one
   worker at a time behind a warm-up barrier (see below).

#### Ordering the hand-off under a worker pool

`RestartMessage` carries `RestartPriority`, which makes the claim query hand it
out before every other row. Under `NumWorkers > 1` that orders the *claims* and
not the *turns*, which is a weaker thing than it sounds: launch the whole pool
at once and one worker takes the restart while a sibling immediately takes the
row behind it, so a normal turn runs against a behavior instance that is still
rebuilding itself from the checkpoint. The documented guarantee that a restart
message is processed before all other messages needs more than a priority to
hold.

So a generation warms up one worker at a time. The supervisor launches a single
worker, and the rest of the pool waits until that worker has resolved the
generation's restart hand-off. For a row supervision enqueued itself (any
supervised restart) the barrier waits for that row unconditionally. For the
boot hand-off, which an owner prepends before `Start` and which the actor
therefore never sees, the barrier orders the first claim instead and releases on
an idle tick, which orders the common case without being able to prove a row was
ever there.

The release rule is shaped so the barrier cannot wedge a pool that has no
hand-off waiting for it:

- A first claim of anything other than a restart message releases the pool
  *before* that message is processed, so a generation with no hand-off pays
  nothing and never serializes behind its first turn.
- A restore that fails is dead-lettered, and a restore that panics tears the
  generation down. Both resolve the hand-off, and both release the pool.
- Whatever ends the warm-up worker releases the pool, including a closed
  mailbox.
- A `Stop` landing mid-barrier releases it through the generation context, so
  shutdown never parks behind a restore.

A single-worker actor is already strictly sequential, so it gets no barrier and
runs the identical code path with nothing to pay for.

#### What "restart" does and does not mean

This is the part worth being precise about, because the name oversells it. The
framework does **not** rebuild the behavior. It stops the workers, optionally
calls `OnStop`, and redelivers a `RestartMessage`; the same Go value keeps
serving afterwards with whatever fields the panic left behind. The clean slate
therefore exists exactly when the actor's `RestartMessage` handler rebuilds
every piece of in-memory state from the durable row, and not otherwise.

Every durable behavior in this repo that carries in-memory state carries a
reload seam for this already, because a rolled-back `Commit` poses the same
problem: `credit.opBehavior` and `oor.sessionBehavior` arm a `commitFailed`
guard that reloads before the next dispatch, `oor.oorRegistryBehavior` re-runs
its non-terminal restore, and `unroll.behavior` re-runs `restoreCheckpoint`.
Their `RestartMessage` handlers use those seams. A behavior with no in-memory
turn state, such as the `serverconn` egress sender, may consume the message as
a no-op, but it should say so and say why rather than leave the reader
guessing.

Two constraints follow for handler authors. The restart message is enqueued
with `MaxAttempts` 1 and the runtime dead-letters rather than nacks a restart
turn that fails (a nacked row at `attempts == max_attempts` is neither
leasable nor reapable, so it would strand in the mailbox forever), which makes
restore handlers responsible for their own idempotency. And `OnStop` now runs
mid-life, once per restart, so it must be idempotent and must leave the
behavior able to serve a new generation rather than assuming it is being
discarded.

An actor whose codec never registered the `RestartMessage` cannot be handed its
checkpoint at all. Supervision checks that up front, before tearing anything
down, and degrades to cycling the worker generation with no `OnStop` and no
restore: a teardown the behavior cannot be rebuilt from is strictly worse than
leaving it running.

#### Identity, promises, and intensity

The actor's public identity is untouched: same ID, same `DurableMailbox`, same
`Ref`. Senders keep enqueueing across the restart gap, and the mailbox's
promise registry survives, so a message that had not yet reached the behavior
is redelivered afterwards and its caller still gets a result. The turns that
were in flight do not survive: the panicking turn's promise is completed with
the panic error, and a sibling worker's turn sees its generation context
cancelled and completes its promise with that context error.

Restarts can be bounded by a BEAM-style intensity budget,
`DurableActorConfig.MaxRestarts` restarts inside `RestartWindow`, tracked in a
sliding window. It defaults **off** (`DefaultMaxRestarts` is
`UnlimitedRestarts`). Restarting forever is no worse than the nack-and-continue
loop supervision replaces, since both are rate-limited by the nack backoff,
whereas a finite budget introduces a failure mode the runtime did not have:
breaking it is terminal, and a terminated actor still holds its ID and its
mailbox rows, so it keeps looking alive to anything that is not watching. Set a
finite budget only where the owner wires `Watch` and reacts to it;
`RecommendedMaxRestarts` (5 per 60s) is the value to reach for when you do.
Breaking the budget makes the actor log at error level, cancel its lifetime
context so further sends fail fast, tear down, and publish its termination.

Supervision also keeps the mailbox tidy across a long run of restarts: each
restart deletes the restart row the previous one enqueued before writing its
own, so at most one restart row is pending at a time rather than one per
restart.

`(*DurableActor).Watch(ctx)` is how another component observes the terminal
event. It returns a channel that receives exactly one `TerminationInfo` and is
then closed, carrying the reason (stopped, context cancelled, restart intensity
exceeded, restart failed), the failure behind it, the lifetime restart count,
and whether the restart budget was exhausted. Delivery is non-blocking by
construction, so a slow watcher can never park the actor's shutdown path.
`Stop` publishes the notification for an actor that was never started, so a
watcher on one does not wait forever.

---

## TypeAssertingRef and MapRef Pattern

This pattern solves a specific problem: the `OutboxPublisher` needs to deliver
messages to actors it discovers at runtime, but Go's type system doesn't allow
direct conversion between generic types.

**When is this used?** Only by the `OutboxPublisher` during CDC message delivery.
Normal actor-to-actor communication (via `Tell`/`Ask` with a known `ActorRef`)
doesn't need this pattern.

**The Problem**: Actors register with concrete types like
`ServiceKey[CounterMessage, int64]`, but the OutboxPublisher uses
`ServiceKey[Message, any]` for type-erased lookups (since it doesn't know the
concrete message type at compile time).

### The Type Mismatch Problem

```mermaid
flowchart TB
    subgraph "Registration Time"
        CA[CounterActor] -->|registers as| SK1["ServiceKey[CounterMessage, int64]"]
    end

    subgraph "Delivery Time"
        OP[OutboxPublisher] -->|looks up| SK2["ServiceKey[Message, any]"]
        SK2 -->|returns| REF["ActorRef[Message, any]"]
        REF -->|needs| TELL["Tell(Message)"]
    end

    subgraph "Problem"
        SK1 -.->|Type mismatch!| SK2
    end
```

### Solution: MapRef and TypeAssertingRef

`MapRef` is a message-transforming wrapper that implements `ActorRef[In, OutR]`
by forwarding to an `ActorRef[Out, InR]` with transformation functions.

`TypeAssertingRef` is a convenience constructor that uses type assertion for
the transformation.

```mermaid
flowchart LR
    subgraph "OutboxPublisher"
        M[Message] --> TAR[TypeAssertingRef]
    end

    subgraph "MapRef Adapter"
        TAR --> |"type assert: Message → CounterMessage"| MR[MapRef]
        MR --> |"forward"| CR["ActorRef[CounterMessage, int64]"]
    end

    subgraph "Target Actor"
        CR --> CA[CounterActor]
    end
```

### How It Works

```go
// TypeAssertingRef creates a MapRef that uses type assertion
func TypeAssertingRef[In Message, Out Message, R any](
    targetRef ActorRef[Out, R],
) *MapRef[In, Out, R, any] {

    return NewMapRef(
        targetRef,
        // mapInput: type assert from In to Out
        func(in In) (Out, error) {
            out, ok := any(in).(Out)
            if !ok {
                var zero Out
                return zero, fmt.Errorf("type assertion failed")
            }
            return out, nil
        },
        // mapOutput: erase result type
        func(r R) any { return r },
    )
}
```

### Registration and Lookup Flow

```mermaid
sequenceDiagram
    participant CA as CounterActor
    participant REC as Receptionist
    participant OP as OutboxPublisher
    participant TAR as TypeAssertingRef

    Note over CA,REC: Registration
    CA->>REC: Register(ServiceKey[CounterMessage, int64], ref)

    Note over OP,TAR: Lookup
    OP->>REC: Lookup(ServiceKey[Message, any], "counter")
    REC-->>OP: ActorRef[Message, any] (via TypeAssertingRef wrapper)

    Note over OP,CA: Delivery
    OP->>TAR: Tell(message: Message)
    TAR->>TAR: Type assert Message → CounterMessage
    TAR->>CA: Tell(CounterMessage)
```

### MapRef Interface Implementation

```go
type MapRef[In, Out Message, InR, OutR any] struct {
    targetRef ActorRef[Out, InR]
    mapInput  func(In) (Out, error)
    mapOutput func(InR) OutR
}

func (m *MapRef) Tell(ctx context.Context, msg In) error {
    transformed, err := m.mapInput(msg)
    if err != nil {
        return fmt.Errorf("map input: %w", err)
    }
    return m.targetRef.Tell(ctx, transformed)
}

func (m *MapRef) Ask(ctx context.Context, msg In) Future[OutR] {
    // Transform input, call inner Ask, transform output
    // ...
}

func (m *MapRef) ID() string {
    return m.targetRef.ID()
}
```

---

## DurableAsk: Crash-Safe Request-Response

Standard Ask uses an in-memory Promise that's lost on crash. DurableAsk solves
this by routing responses through the durable outbox/mailbox infrastructure.

**Two key parameters** enable the async response flow:

- **CallbackActorID**: The caller's actor ID. The target writes the response to
  its outbox with this as the destination. The OutboxPublisher routes it to the
  caller's mailbox.

- **CorrelationID**: A unique ID generated by the caller to match responses to
  requests. Since DurableAsk is async (response arrives later as a separate
  message), the caller may have multiple outstanding requests. The CorrelationID
  lets it know which request each response corresponds to.

See the [Developer Guide](durable_actor_quickstart.md#durableask-crash-safe-request-response)
for implementation details.

### Ask vs DurableAsk

```mermaid
flowchart TB
    subgraph "Standard Ask"
        A1[Caller] -->|Ask| T1[Target]
        T1 -->|Complete Promise| P1[In-Memory Promise]
        P1 -->|Await| A1
        style P1 fill:#f88,stroke:#333
        Note1[Lost on crash!]
    end

    subgraph "DurableAsk"
        A2[Caller] -->|DurableAsk| T2[Target]
        T2 -->|Write to outbox| O2[(outbox_messages)]
        O2 -->|OutboxPublisher| M2[(caller's mailbox)]
        M2 -->|Receive| A2
        style O2 fill:#8f8,stroke:#333
        style M2 fill:#8f8,stroke:#333
        Note2[Survives crashes!]
    end
```

### DurableAsk Sequence

```mermaid
sequenceDiagram
    participant C as Caller Actor
    participant CM as Caller Mailbox
    participant TM as Target Mailbox
    participant T as Target Actor
    participant OB as outbox_messages
    participant OP as OutboxPublisher

    Note over C,T: Request Phase
    C->>TM: DurableAsk(msg, callback_id=C, correlation_id=123)
    Note over TM: Message persisted with callback metadata

    T->>TM: LeaseNextMessage()
    TM-->>T: Message with callback_actor_id, correlation_id
    T->>T: Process message

    Note over T,OP: Response Phase
    T->>OB: EnqueueOutbox(AskResponse{correlation_id=123, result})
    T->>TM: Ack message

    OP->>OB: ClaimOutboxBatch()
    OP->>CM: Tell(AskResponse)

    Note over C: Response Delivery
    C->>CM: Receive()
    CM-->>C: AskResponse{correlation_id=123, result}
    C->>C: Match by correlation_id
```

### AskResponse Structure

```go
type AskResponse struct {
    CorrelationID string    // Links to original request
    ResultBlob    tlv.Blob  // Encoded result (nil if error)
    ErrorText     string    // Error message (empty if success)
}

// Helper to decode the result
func (m AskResponse) DecodeResult(codec *MessageCodec) (TLVMessage, error)
```

### Crash Recovery with DurableAsk

```mermaid
flowchart TD
    subgraph "Before Crash"
        A[Caller sends DurableAsk] --> B[Target processes]
        B --> C[Target writes AskResponse to outbox]
        C --> D[Caller crashes!]
    end

    subgraph "After Recovery"
        E[Caller restarts] --> F[Resume mailbox processing]
        F --> G[OutboxPublisher delivers AskResponse]
        G --> H[Caller receives response]
        H --> I[Match by correlation_id]
    end

    D -.->|Time passes| E
```

### When to Use DurableAsk

| Scenario | Use |
|----------|-----|
| Quick operations, caller won't crash | Standard Ask |
| Long-running operations | DurableAsk |
| Caller may crash before response | DurableAsk |
| Response must survive restarts | DurableAsk |
| Fire-and-forget | Tell |

---

## Summary

The durable actor architecture provides crash-resilient message processing
through:

1. **Inbox Durability**: Messages persisted before processing
2. **Transactional Outbox**: CDC pattern for atomic state + message writes
3. **Lease-Based Delivery**: Prevents stale acks, enables automatic redelivery
4. **Deduplication**: Exactly-once processing semantics
5. **Checkpointing**: FSM state recovery on restart
6. **TypeAssertingRef**: Type-safe actor discovery at runtime
7. **DurableAsk**: Crash-safe request-response pattern

These patterns combine to provide strong delivery guarantees while maintaining
the simplicity of the actor programming model.
