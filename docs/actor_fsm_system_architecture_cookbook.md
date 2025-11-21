# Actor/FSM System Architecture Cookbook

Comprehensive guide for building restart-safe, idempotent, side-effect-free
workflows using `baselib/protofsm` and `baselib/actor`. This document defines
architectural patterns and rules for implementing FSM-based subsystems with
support for both hierarchical push models and pub/sub event sourcing.

**Target Audience:** Engineers and AI agents implementing new FSMs, actors, and
persistence layers.

## Table of Contents

1. [System Overview](#system-overview)
2. [Core Components](#core-components)
3. [Layered Responsibility Model](#layered-responsibility-model)
4. [Decision Trees](#decision-trees)
5. [Durability and Persistence](#durability-and-persistence)
6. [Idempotency and Determinism](#idempotency-and-determinism)
7. [Communication Patterns](#communication-patterns)
8. [Implementation Checklist](#implementation-checklist)
9. [Anti-Patterns and Pitfalls](#anti-patterns-and-pitfalls)
10. [Concrete Examples](#concrete-examples)

---

## System Overview

### Architecture Philosophy

The system is built on three foundational principles:

1. **Side-Effect Freedom**: FSM state transitions are pure computations. All
   side effects (I/O, actor calls, persistence) are expressed as declarative
   outbox events that are dispatched after the state transition completes.

2. **Idempotency**: Every operation (state transitions, outbox events, actor
   messages) must be safe to execute multiple times. Replaying the same event
   sequence from init state always produces the same final state.

3. **Flexibility**: The architecture supports both hierarchical push models
   (direct actor-to-actor communication) and pub/sub event sourcing
   (consumers track their own cursors on an event stream).

### What Exists in `baselib/`

- **`protofsm.StateMachine`**: Pure event-driven state machine executor.
  Consumes events, transitions states, emits internal events (for nested
  transitions) and outbox events (for external side effects).

- **`protofsm.ActorStateMachine`**: Wraps a `StateMachine` as an actor
  behavior. Automatically dispatches outbox events to the `ActorSystem` using
  service keys.

- **`actor.ActorSystem`**: Manages actor lifecycle, mailboxes, service
  registry, and message routing. Provides the runtime for all actors.

- **`actor.Router`**: Routes messages to actors registered under a service key.
  Uses round-robin by default; custom routing strategies can be implemented.

- **Environment**: Injected dependency container for FSM states. Provides cheap,
  repeatable helpers (time, config, pure calculations). Can optionally hold the
  FSM's own actor reference for self-callbacks.

---

## Core Components

### FSM (Finite State Machine)

**Purpose:** Deterministic state transition logic.

**Responsibilities:**
- Process events and produce state transitions
- Emit internal events (for nested state transitions)
- Emit outbox events (declare side effects without executing them)
- Maintain in-memory state (must be serializable for durability)

**Constraints:**
- **Pure computation only** - no I/O, no network calls, no database writes
- **Deterministic** - same input sequence produces same output state
- **State isolated in state objects** - no hidden mutable state outside FSM state

**Example:**
```go
func (s *StateAwaitingReview) ProcessEvent(ctx context.Context, event DocEvent,
	env *DocEnvironment) (*protofsm.StateTransition[DocEvent, DocOutboxEvent, *DocEnvironment], error) {

	switch e := event.(type) {
	case EventApproved:
		// Pure computation: create next state
		nextState := &StateApproved{
			documentID: s.documentID,
			approvedBy: e.Reviewer,
			approvedAt: env.Clock.Now(), // Inject time via env
		}

		// Declare side effects via outbox events
		return &protofsm.StateTransition[...]{
			NextState: nextState,
			NewEvents: fn.Some(protofsm.EmittedEvent[...]{
				Outbox: []DocOutboxEvent{
					NewOutboxPersistState(nextState),  // Persist
					NewOutboxNotifyApproval(s.documentID), // Notify
				},
			}),
		}, nil
	}
}
```

### Outbox Event

**Purpose:** Declare a side effect to be executed after state transition.

**Responsibilities:**
- Describe **what** side effect to perform and **who** should execute it
- Know the target service key and message payload
- Be dispatchable by the wrapper actor or dispatcher

**Constraints:**
- **Idempotent** - safe to dispatch multiple times
- **Self-contained** - includes all information needed for dispatch
- **Never performs work itself** - only describes work to be done

**Types:**
- **Tell (fire-and-forget)**: Non-blocking, no response expected
- **Ask (request-response)**: Blocking, waits for response

**Example:**
```go
// Outbox event declaration
type OutboxPersistState struct {
	protofsm.RoutedOutboxEvent[PersistMsg, PersistResp]
}

func NewOutboxPersistState(state DocState) OutboxPersistState {
	return OutboxPersistState{
		RoutedOutboxEvent: protofsm.NewAskOutboxEvent(
			PersistServiceKey,  // Where to send
			PersistMsg{         // What to send
				StateID: state.ID(),
				StateData: state.Serialize(),
				IdempotencyKey: state.EventCursor(),
			},
			),
		}
	}

// No additional methods required: RoutedOutboxEvent already implements
// protofsm.ActorOutboxEvent and knows how to dispatch via actor.Router.
```

### Environment

**Purpose:** Provide dependencies and helpers to FSM state transitions.

**Responsibilities:**
- Expose cheap, repeatable, side-effect-free helpers (time, config, caches)
- Optionally hold the FSM's own actor reference for self-callbacks
- Provide deterministic inputs for otherwise non-deterministic operations

**Constraints:**
- **No foreign actor refs** - other actors are reached via outbox events
- **Repeatable** - calling same env method with same input produces same output
- **Minimal side effects** - only for operations safe to repeat (cache reads)

**Example:**
```go
type SwapEnvironment struct {
	// Cheap, repeatable helpers
	Clock      clock.Clock      // Mockable time
	Config     *SwapConfig      // Static configuration
	FeeCalc    *FeeCalculator   // Pure calculation

	// Self-reference for callbacks
	actorRef actor.TellOnlyRef[protofsm.ActorMessage[SwapEvent]]

	// Allowed: cheap, idempotent reads
	priceCache *Cache[Price]  // Safe to query multiple times
}

// Injected by ActorStateMachine when Env implements TellRefEnv.
func (e *SwapEnvironment) SetTellOnlyRef(
	ref actor.TellOnlyRef[protofsm.ActorMessage[SwapEvent]],
) {
	e.actorRef = ref
}

func (e *SwapEnvironment) GetTellOnlyRef() actor.TellOnlyRef[
	protofsm.ActorMessage[SwapEvent]] {

	return e.actorRef
}

// Compile-time check.
var _ protofsm.TellRefEnv[SwapEvent] = (*SwapEnvironment)(nil)

// Pure helper: safe to call multiple times
func (e *SwapEnvironment) CalculateFee(amount btcutil.Amount) btcutil.Amount {
	return e.FeeCalc.Calculate(amount, e.Config.FeeRate)
}

// Idempotent read: safe to call multiple times
func (e *SwapEnvironment) GetCurrentPrice() (Price, error) {
	return e.priceCache.Get(e.Clock.Now())
}
```

### Wrapping Actor

**Purpose:** Own the FSM instance and mediate its interactions with the system.

**Responsibilities:**
- Spawn and manage the `ActorStateMachine`
- Receive external messages and forward them to the FSM as events
- Persist state transitions and outbox events (atomically)
- Dispatch outbox events to target actors (after persistence)
- Handle errors and implement retry/backoff logic

**Constraints:**
- **FSM owner** - one FSM per wrapping actor
- **Persistence coordinator** - ensures durability before dispatch
- **Error boundary** - handles FSM errors and reports them

**Example:**
```go
type SwapManager struct {
	system     *actor.ActorSystem
	store      SwapStore
	fsmActors  map[string]actor.ActorRef[protofsm.ActorMessage[SwapEvent], ...]
	mu         sync.RWMutex
}

func (m *SwapManager) StartSwap(ctx context.Context, id string) error {
	// Create environment
	env := &SwapEnvironment{
		Clock:   clock.RealClock{},
		Config:  m.config,
		FeeCalc: m.feeCalculator,
	}

	// Create FSM config
	cfg := protofsm.StateMachineCfg[SwapEvent, SwapOutboxEvent, *SwapEnvironment]{
		Logger:       m.logger.WithPrefix(id),
		InitialState: &StateInit{id: id},
		Env:          env,
	}

	// Spawn FSM as actor (wrapping happens here)
	fsmRef := protofsm.NewSystemsActorStateMachine(ctx, cfg, m.system, id)

	// Track actor reference
	m.mu.Lock()
	m.fsmActors[id] = fsmRef
	m.mu.Unlock()

	// Send initial event
	fsmRef.Tell(ctx, protofsm.ActorMessage[SwapEvent]{
		Event: EventStart{ID: id},
	})

	return nil
}

func (m *SwapManager) ResumeSwaps(ctx context.Context) error {
	// Load pending swaps from storage
	swaps, err := m.store.ListPending(ctx)
	if err != nil {
		return err
	}

	for _, swap := range swaps {
		// Reconstruct state from storage
		state, err := m.reconstructState(swap)
		if err != nil {
			return err
		}

		// Spawn FSM with persisted state
		cfg := protofsm.StateMachineCfg[SwapEvent, SwapOutboxEvent, *SwapEnvironment]{
			Logger:       m.logger.WithPrefix(swap.ID),
			InitialState: state,
			Env:          m.createEnv(),
		}

		fsmRef := protofsm.NewSystemsActorStateMachine(ctx, cfg, m.system, swap.ID)

		m.mu.Lock()
		m.fsmActors[swap.ID] = fsmRef
		m.mu.Unlock()

		// Re-establish pending operations
		fsmRef.Tell(ctx, protofsm.ActorMessage[SwapEvent]{
			Event: EventResume{},
		})
	}

	return nil
}
```

### Service Actor

**Purpose:** Execute side effects declared by outbox events.

**Responsibilities:**
- Process messages from outbox events
- Execute I/O operations (network, disk, blockchain)
- Send result events back to FSMs
- Implement idempotency (handle duplicate messages)

**Constraints:**
- **Idempotent** - receiving same message twice is safe
- **Stateless preferred** - or maintain minimal state for deduplication
- **Error handling** - return errors via result types, don't panic

**Example:**
```go
// Service key definition
const persistActorName = "persist"
var PersistServiceKey = actor.NewServiceKey[PersistMsg, PersistResp](persistActorName)

// Message types
type PersistMsg struct {
	actor.BaseMessage
	StateID        string
	StateData      []byte
	IdempotencyKey string  // For deduplication
}

func (m PersistMsg) MessageType() string { return "PersistState" }

type PersistResp struct {
	Success bool
}

// Actor behavior
type PersistActor struct {
	store       SwapStore
	dedupCache  *Cache[string]  // Track processed idempotency keys
}

func (a *PersistActor) Receive(ctx context.Context, msg PersistMsg) fn.Result[PersistResp] {
	// Idempotency: check if already processed
	if a.dedupCache.Has(msg.IdempotencyKey) {
		return fn.Ok(PersistResp{Success: true})
	}

	// Perform persistence
	if err := a.store.UpsertSwap(ctx, msg.StateID, msg.StateData); err != nil {
		return fn.Err[PersistResp](err)
	}

	// Track as processed
	a.dedupCache.Put(msg.IdempotencyKey, true)

	return fn.Ok(PersistResp{Success: true})
}
```

---

## Layered Responsibility Model

The system follows a strict **hierarchical layering** where each level only
knows about the level directly below it. This enables composition and prevents
tight coupling.

### Hierarchy

```
┌─────────────────────────────────────────────────────────┐
│ System Manager                                          │
│ - Bootstraps ActorSystem                                │
│ - Spawns FSM managers                                   │
│ - Handles system-wide restart and shutdown              │
└────────────────────┬────────────────────────────────────┘
                     │ knows about ▼
┌────────────────────┴────────────────────────────────────┐
│ FSM Manager / Wrapping Actor                            │
│ - Spawns and tracks FSM actors                          │
│ - Coordinates persistence                               │
│ - Implements business logic orchestration               │
└────────────────────┬────────────────────────────────────┘
                     │ knows about ▼
┌────────────────────┴────────────────────────────────────┐
│ ActorStateMachine (FSM Actor)                           │
│ - Wraps StateMachine as actor behavior                  │
│ - Dispatches outbox events to ActorSystem               │
│ - Handles event→state transitions                       │
└────────────────────┬────────────────────────────────────┘
                     │ knows about ▼
┌────────────────────┴────────────────────────────────────┐
│ StateMachine (FSM Core)                                 │
│ - Processes events deterministically                    │
│ - Emits internal events and outbox events               │
│ - Pure state transition logic only                      │
└────────────────────┬────────────────────────────────────┘
                     │ knows about ▼
┌────────────────────┴────────────────────────────────────┐
│ States, Events, Environment                             │
│ - State: transition functions                           │
│ - Events: sealed union types                            │
│ - Environment: repeatable helpers                       │
└─────────────────────────────────────────────────────────┘
```

### Interaction Rules

1. **FSMs never call other actors directly**
   - FSMs emit outbox events
   - Wrapping actor or dispatcher converts outbox events to actor calls

2. **Actors can send events to FSMs**
   - Actors receive FSM's `TellOnlyRef` in their message
   - After completing work, actors send result events back to FSM

3. **Managers orchestrate**
   - Managers know the broader system topology
   - Managers spawn FSMs and wire up actors
   - Managers implement cross-FSM coordination (if needed)

4. **Environment provides local context only**
   - No foreign actor refs (except optionally the FSM's own ref)
   - Pure helpers, time, config, cached reads only

### Example: Server Out-Swap Hierarchy

```
ServerManager (knows about all swap FSMs)
  │
  ├─ ServerOutSwapActor (wraps OutSwap FSM)
  │   │
  │   └─ OutSwap FSM (emits outbox events)
  │       ├─ StateInit → OutboxMonitorHTLC
  │       ├─ StateMonitoring → OutboxFundSwap
  │       └─ StateFunded → OutboxSettleHTLC
  │
  ├─ HTLCInterceptorActor (service actor)
  ├─ FundingActor (service actor)
  └─ PersistActor (service actor)
```

**Event flow:**
1. OutSwap FSM transitions to `StateFunded`
2. FSM emits `OutboxSettleHTLC` (no I/O, pure declaration)
3. `ActorStateMachine` dispatches outbox event
4. `HTLCInterceptorActor` receives message, settles HTLC on Lightning Network
5. `HTLCInterceptorActor` sends `EventHTLCSettled` back to OutSwap FSM
6. OutSwap FSM transitions to `StateCompleted`

---

## Decision Trees

### When to use Env vs Outbox vs Direct Actor Call?

```
Is the operation side-effect free and cheap?
│
├─ YES: Use env call
│  └─ Examples: time, config read, pure calculation, cache lookup
│
└─ NO: Does it perform external I/O or change observable state?
   │
   ├─ YES: Use outbox event
   │  └─ Examples: database write, network call, other actor interaction
   │
   └─ NO: Is this a wrapping actor/manager coordinating FSMs?
      │
      ├─ YES: Direct actor call (via Router or ActorRef)
      │  └─ Examples: manager spawning FSMs, cross-FSM coordination
      │
      └─ NO: You're probably in an FSM → use outbox event
```

### When to use Tell vs Ask for outbox events?

```
Does the FSM need the result to proceed?
│
├─ YES: Use Ask (request-response)
│  └─ Examples: persistence (must know if successful), validation
│
└─ NO: Is the operation fire-and-forget?
   │
   ├─ YES: Use Tell (fire-and-forget)
   │  └─ Examples: monitoring, notifications, background tasks
   │
   └─ MAYBE: Will the result arrive as a separate event later?
      │
      └─ YES: Use Tell + callback pattern
         └─ Actor sends result event to FSM's TellOnlyRef later
         └─ Examples: async monitoring, blockchain confirmation
```

### When to use EventResume?

```
Can your FSM be persisted and restarted?
│
├─ YES: Implement EventResume in all non-terminal states
│  │
│  └─ EventResume should re-emit outbox events to re-establish:
│     ├─ Monitoring subscriptions
│     ├─ Background tasks
│     └─ Any async operations that were in flight
│
└─ NO: Your FSM is ephemeral (rare)
   └─ Only for truly transient workflows
```

### When to use Push (actor-to-actor) vs Pub/Sub (event sourcing)?

```
How are consumers determined?
│
├─ KNOWN AT WRITE TIME: Use push (actor-to-actor)
│  │
│  ├─ FSM knows exactly which actor(s) need to be notified
│  ├─ Outbox event specifies target service key
│  └─ Examples: swap FSM → monitoring actor, order FSM → execution actor
│
└─ UNKNOWN AT WRITE TIME: Use pub/sub (event sourcing)
   │
   ├─ Multiple consumers may subscribe to state change events
   ├─ New consumers can be added without changing FSM
   ├─ Each consumer tracks its own cursor in event stream
   └─ Examples: audit log, analytics, notifications to multiple services
```

**Hybrid approach:** Both can coexist!
- FSM emits push outbox events for critical operations
- FSM also writes state transitions to event stream table
- Consumers choose to listen to push events OR poll event stream

---

## Durability and Persistence

### Core Principle: Persist-Before-Notify

**Rule:** Commit state + outbox events to durable storage **before** dispatching
them to the actor system.

**Why?** Crash safety. If the system crashes after dispatching but before
persisting, work will be announced that isn't durable.

**Implementation:**
```go
// CORRECT: Persist then dispatch
func (m *SwapManager) ProcessEvent(ctx context.Context, swapID string, event SwapEvent) error {
	// 1. Apply event to FSM (in-memory only).
	// This helper wraps StateMachine.AskEvent + CurrentState.
	newState, outbox, err := m.applyEvent(ctx, swapID, event)
	if err != nil {
		return err
	}

	// 2. Persist state + outbox in single transaction
	if err := m.store.AtomicWrite(ctx, func(tx *Tx) error {
		// Write state snapshot
		if err := tx.UpsertSwapState(swapID, newState); err != nil {
			return err
		}

		// Write outbox entries
		for _, evt := range outbox {
			if err := tx.InsertOutboxEntry(swapID, evt); err != nil {
				return err
			}
		}

		return nil
	}); err != nil {
		return err
	}

	// 3. ONLY AFTER COMMIT: dispatch outbox events
	for _, evt := range outbox {
		if err := evt.Dispatch(ctx, m.system); err != nil {
			// If dispatch fails, outbox entry remains in DB for retry
			m.logger.ErrorS(ctx, "Failed to dispatch outbox event", err)
		}
	}

	return nil
}
```

### Event Log with Cursor

Store transitions as an append-only event log with a monotonically increasing
cursor.

**Schema:**
```sql
CREATE TABLE swap_events (
	swap_id      TEXT NOT NULL,
	event_cursor INTEGER NOT NULL,  -- Monotonic counter
	event_type   TEXT NOT NULL,
	event_data   JSONB NOT NULL,
	created_at   TIMESTAMP NOT NULL DEFAULT NOW(),
	PRIMARY KEY (swap_id, event_cursor)
);

CREATE TABLE swap_states (
	swap_id      TEXT PRIMARY KEY,
	event_cursor INTEGER NOT NULL,      -- Last processed event
	state_name   TEXT NOT NULL,
	state_data   JSONB NOT NULL,
	updated_at   TIMESTAMP NOT NULL DEFAULT NOW(),
	FOREIGN KEY (swap_id, event_cursor) REFERENCES swap_events(swap_id, event_cursor)
);

CREATE TABLE swap_outbox (
	id           SERIAL PRIMARY KEY,
	swap_id      TEXT NOT NULL,
	event_cursor INTEGER NOT NULL,      -- Which transition produced this
	outbox_type  TEXT NOT NULL,
	outbox_data  JSONB NOT NULL,
	dispatched   BOOLEAN NOT NULL DEFAULT FALSE,
	dispatched_at TIMESTAMP,
	created_at   TIMESTAMP NOT NULL DEFAULT NOW(),
	FOREIGN KEY (swap_id, event_cursor) REFERENCES swap_events(swap_id, event_cursor)
);
```

**Transaction:**
```go
func (s *Store) AppendEvent(ctx context.Context, swapID string, event SwapEvent,
	newState SwapState, outbox []SwapOutboxEvent) error {

	return s.db.InTx(ctx, func(tx *Tx) error {
		// Get current cursor
		cursor, err := tx.GetCurrentCursor(swapID)
		if err != nil {
			return err
		}
		nextCursor := cursor + 1

		// Append event to log
		if err := tx.InsertEvent(swapID, nextCursor, event); err != nil {
			return err
		}

		// Update state snapshot with new cursor
		if err := tx.UpsertState(swapID, nextCursor, newState); err != nil {
			return err
		}

		// Insert outbox entries
		for _, evt := range outbox {
			if err := tx.InsertOutboxEntry(swapID, nextCursor, evt); err != nil {
				return err
			}
		}

		return nil
	})
}
```

### Resume Flow

On restart:

1. **Load state from storage**
   ```go
   state, cursor, err := store.GetSwapState(ctx, swapID)
   ```

2. **Spawn FSM with loaded state**
   ```go
   cfg := protofsm.StateMachineCfg[...]{
       InitialState: state,  // From storage
       Env:          env,
   }
   fsmRef := protofsm.NewSystemsActorStateMachine(ctx, cfg, system, swapID)
   ```

3. **Replay unflushed outbox entries**
   ```go
   unflushed, err := store.GetUnflushedOutbox(ctx, swapID)
   for _, entry := range unflushed {
       entry.Dispatch(ctx, system)
       store.MarkDispatched(ctx, entry.ID)
   }
   ```

4. **Send EventResume to re-establish monitors**
   ```go
   fsmRef.Tell(ctx, protofsm.ActorMessage[SwapEvent]{
       Event: EventResume{},
   })
   ```

### Pub/Sub Event Sourcing

For consumers that track their own cursors:

**Event Stream Table:**
```sql
CREATE TABLE swap_event_stream (
	swap_id      TEXT NOT NULL,
	event_cursor INTEGER NOT NULL,
	event_type   TEXT NOT NULL,
	state_before TEXT NOT NULL,
	state_after  TEXT NOT NULL,
	event_data   JSONB NOT NULL,
	created_at   TIMESTAMP NOT NULL DEFAULT NOW(),
	PRIMARY KEY (swap_id, event_cursor)
);

CREATE INDEX idx_swap_event_stream_cursor ON swap_event_stream(event_cursor);
```

**Consumer Cursor Table:**
```sql
CREATE TABLE consumer_cursors (
	consumer_id  TEXT PRIMARY KEY,
	last_cursor  INTEGER NOT NULL DEFAULT 0,
	updated_at   TIMESTAMP NOT NULL DEFAULT NOW()
);
```

**Consumer Pattern:**
```go
type AuditConsumer struct {
	store ConsumerStore
	id    string
}

func (c *AuditConsumer) Poll(ctx context.Context) error {
	// Get last processed cursor
	cursor, err := c.store.GetCursor(ctx, c.id)
	if err != nil {
		return err
	}

	// Fetch new events
	events, err := c.store.GetEventsAfter(ctx, cursor, 100)
	if err != nil {
		return err
	}

	// Process events (idempotent)
	for _, evt := range events {
		if err := c.processEvent(ctx, evt); err != nil {
			return err  // Stop on error, will retry on next poll
		}

		// Advance cursor only after successful processing
		if err := c.store.SetCursor(ctx, c.id, evt.Cursor); err != nil {
			return err
		}
	}

	return nil
}
```

---

## Idempotency and Determinism

### Determinism: Same Input → Same Output

**Rule:** Replaying the same event sequence from init state must always produce
the same final state.

**Requirements:**

1. **Pure FSM logic** - no hidden state, no I/O in ProcessEvent
2. **Inject non-determinism via env** - time, randomness, external queries
3. **Memoize env results per event** - same event → same env responses

**Example:**
```go
// WRONG: Non-deterministic
func (s *StateInit) ProcessEvent(ctx context.Context, event Event, env *Env) (...) {
	now := time.Now()  // ❌ Different on replay!
	random := rand.Int()  // ❌ Different on replay!
	price, _ := http.Get("api/price")  // ❌ External call!
	// ...
}

// RIGHT: Deterministic
func (s *StateInit) ProcessEvent(ctx context.Context, event Event, env *Env) (...) {
	now := env.Clock.Now()  // ✅ Mockable, repeatable
	random := env.GetEventRandom(event)  // ✅ Seeded per event
	price := env.GetPrice()  // ✅ Cached, deterministic
	// ...
}
```

### Idempotency: Same Action → Same Effect

**Rule:** Every outbox event and actor message must be safe to process multiple
times without causing incorrect state or duplicate effects.

**Techniques:**

1. **Idempotency Keys**
   ```go
   type PersistMsg struct {
       StateID        string
       IdempotencyKey string  // swapID + eventCursor
       StateData      []byte
   }

   func (a *PersistActor) Receive(ctx context.Context, msg PersistMsg) fn.Result[...] {
       // Check if already processed
       if a.dedupCache.Has(msg.IdempotencyKey) {
           return fn.Ok(PersistResp{Success: true})
       }

       // Process and mark as done
       if err := a.store.Upsert(ctx, msg.StateID, msg.StateData); err != nil {
           return fn.Err[PersistResp](err)
       }

       a.dedupCache.Put(msg.IdempotencyKey, true)
       return fn.Ok(PersistResp{Success: true})
   }
   ```

2. **Natural Idempotency (Upsert)**
   ```go
   // Upsert is naturally idempotent - executing twice is safe
   func (s *Store) UpsertSwap(ctx context.Context, id string, state State) error {
       _, err := s.db.Exec(ctx, `
           INSERT INTO swaps (id, state_name, state_data, updated_at)
           VALUES ($1, $2, $3, NOW())
           ON CONFLICT (id) DO UPDATE
           SET state_name = EXCLUDED.state_name,
               state_data = EXCLUDED.state_data,
               updated_at = NOW()
       `, id, state.Name(), state.Data())
       return err
   }
   ```

3. **State-Based Checks**
   ```go
   func (s *StateMonitoring) ProcessEvent(ctx context.Context, event Event, env *Env) (...) {
       switch e := event.(type) {
       case EventFunded:
           // Idempotent: only transition if in correct state
           if s.funded {
               // Already processed this event, no-op
               return &protofsm.StateTransition[...]{NextState: s}, nil
           }

           // First time seeing this event
           s.funded = true
           return &protofsm.StateTransition[...]{
               NextState: &StateFunded{...},
               // ...
           }, nil
       }
   }
   ```

4. **At-Least-Once Delivery**
   - Design all actors to handle duplicate messages gracefully
   - Use deduplication caches, version checks, or natural idempotency
   - Prefer "process twice" over "process never" (availability over consistency)

---

## Communication Patterns

### Pattern 1: Push (Actor-to-Actor via Outbox)

**When:** FSM knows exactly which actor(s) should be notified.

**Flow:**
1. FSM emits outbox event with target service key
2. ActorStateMachine dispatches to service key
3. Router routes to registered actor(s)
4. Actor processes message, may send result event back to FSM

**Example:**
```go
// FSM emits
NewOutboxMonitorHTLC(htlcID, env.actorRef)

// Dispatches to
var HTLCMonitorServiceKey = actor.NewServiceKey[MonitorHTLCMsg, MonitorHTLCResp]("htlc_monitor")

// Actor receives
func (a *HTLCMonitorActor) Receive(ctx context.Context, msg MonitorHTLCMsg) fn.Result[...] {
	go func() {
		// Monitor HTLC in background
		result := a.waitForHTLC(ctx, msg.HTLCID)

		// Send result back to FSM
		msg.FsmRef.Tell(ctx, protofsm.ActorMessage[SwapEvent]{
			Event: EventHTLCConfirmed{HTLCID: msg.HTLCID, Result: result},
		})
	}()

	return fn.Ok(MonitorHTLCResp{Success: true})
}
```

**Advantages:**
- Direct, low-latency communication
- Explicit dependencies (FSM knows who it talks to)
- Strong typing (service key enforces message types)

**Disadvantages:**
- Tight coupling (FSM must know all consumers)
- Hard to add new consumers without modifying FSM

### Pattern 2: Pub/Sub (Event Sourcing via Event Stream)

**When:** Multiple unknown consumers want to react to FSM state changes.

**Flow:**
1. FSM transitions state
2. Persistence layer writes state transition to event stream table
3. Consumers poll event stream, tracking their own cursors
4. Each consumer processes events idempotently

**Example:**
```go
// Persistence layer writes to event stream
func (s *Store) UpsertSwapState(ctx context.Context, swapID string, state State, cursor int) error {
	return s.db.InTx(ctx, func(tx *Tx) error {
		// Update state snapshot
		if err := tx.UpsertState(swapID, state); err != nil {
			return err
		}

		// Write to event stream (for pub/sub consumers)
		if err := tx.InsertEventStreamEntry(swapID, cursor, state.Event()); err != nil {
			return err
		}

		return nil
	})
}

// Consumer 1: Audit logger
type AuditConsumer struct {
	id string
}

func (c *AuditConsumer) Poll(ctx context.Context) error {
	cursor := c.getCursor()
	events := c.fetchEvents(cursor)

	for _, evt := range events {
		c.auditLog.Write(evt)
		c.setCursor(evt.Cursor)
	}

	return nil
}

// Consumer 2: Analytics
type AnalyticsConsumer struct {
	id string
}

func (c *AnalyticsConsumer) Poll(ctx context.Context) error {
	cursor := c.getCursor()
	events := c.fetchEvents(cursor)

	for _, evt := range events {
		c.analyticsDB.Insert(evt)
		c.setCursor(evt.Cursor)
	}

	return nil
}
```

**Advantages:**
- Loose coupling (FSM doesn't know consumers exist)
- Easy to add new consumers without changing FSM
- Consumers progress independently (no backpressure)

**Disadvantages:**
- Higher latency (polling or push notifications required)
- Eventual consistency (consumers lag behind FSM)

### Pattern 3: Hybrid (Push + Pub/Sub)

**Best of both worlds:**
- Use push for critical, latency-sensitive operations
- Use pub/sub for audit, analytics, non-critical notifications

**Example:**
```go
func (s *StateMonitoring) ProcessEvent(...) (...) {
	case EventFunded:
		nextState := &StateFunded{...}

		return &protofsm.StateTransition[...]{
			NextState: nextState,
			NewEvents: fn.Some(protofsm.EmittedEvent[...]{
				Outbox: []SwapOutboxEvent{
					// Push: critical persistence (Ask pattern)
					NewOutboxPersistState(nextState),

					// Push: critical notification (Tell pattern)
					NewOutboxNotifyFunded(s.swapID, env.actorRef),
				},
			}),
		}, nil
}

// Persistence layer also writes to event stream
func (s *Store) UpsertSwapState(...) error {
	// ... upsert state snapshot ...

	// Pub/sub: audit/analytics consumers poll this
	s.eventStream.Append(swapID, cursor, StateTransitionEvent{
		StateFrom: oldState,
		StateTo:   newState,
		Event:     event,
	})

	return nil
}
```

---

## Implementation Checklist

### For Each FSM

- [ ] **Define sealed event types**
  - [ ] Create sealed interface with `isEventSealed()` method
  - [ ] Define all possible events (include `EventResume`)
  - [ ] Events are immutable data structures (no methods beyond sealing)

- [ ] **Define sealed outbox event types**
  - [ ] Create sealed interface implementing `protofsm.ActorOutboxEvent`
  - [ ] Use `protofsm.RoutedOutboxEvent` for actor-routed events
  - [ ] Include idempotency keys in message payloads
  - [ ] Decide Tell vs Ask for each outbox event type

- [ ] **Define sealed state types**
  - [ ] Create sealed interface implementing `protofsm.State[...]`
  - [ ] Each state is a struct with data fields (no methods beyond interface)
  - [ ] Implement `ProcessEvent(...)` for each state
  - [ ] Mark terminal states with `IsTerminal() = true`
  - [ ] Implement `EventResume` in all non-terminal states

- [ ] **Design environment**
  - [ ] Create env struct with only repeatable helpers
  - [ ] Inject time, config, pure calculators
  - [ ] Optionally implement `TellRefEnv` or `FullRefEnv`
  - [ ] No foreign actor refs (only self-ref allowed)

- [ ] **Ensure determinism**
  - [ ] No I/O in `ProcessEvent` methods
  - [ ] No `time.Now()`, use `env.Clock.Now()`
  - [ ] No `rand.Int()`, use seeded/memoized randomness from env
  - [ ] All external queries go through repeatable env methods

- [ ] **Emit outbox events correctly**
  - [ ] Side effects declared as outbox events, never executed inline
  - [ ] Include all necessary data in outbox event (self-contained)
  - [ ] Include idempotency key or version for deduplication

### For Each Service Actor

- [ ] **Define service key**
  - [ ] Use `actor.NewServiceKey[MsgType, RespType](name)`
  - [ ] Document what the actor does

- [ ] **Define message types**
  - [ ] Embed `actor.BaseMessage`
  - [ ] Implement `MessageType() string`
  - [ ] Include idempotency key if needed
  - [ ] Include FSM `TellOnlyRef` if actor needs to send events back

- [ ] **Implement Receive method**
  - [ ] Return `fn.Result[RespType]`
  - [ ] Implement idempotency (dedup cache, upsert, state checks)
  - [ ] Send result events back to FSM if needed
  - [ ] Handle errors gracefully (return fn.Err, don't panic)

- [ ] **Register with ActorSystem**
  - [ ] `actor.RegisterWithSystem(system, name, serviceKey, behavior)`

### For Wrapping Actor / Manager

- [ ] **Bootstrap ActorSystem**
  - [ ] Create with appropriate config (mailbox size, etc.)
  - [ ] Register all service actors

- [ ] **Spawn FSM actors**
  - [ ] Create environment
  - [ ] Create FSM config with initial state
  - [ ] Use `protofsm.NewSystemsActorStateMachine(...)`
  - [ ] Track actor refs in manager (map of ID → ActorRef)

- [ ] **Implement persistence hooks**
  - [ ] Persist state + outbox in single transaction
  - [ ] Dispatch outbox events only after commit
  - [ ] Handle dispatch failures (retry with backoff)

- [ ] **Implement resume logic**
  - [ ] Load pending workflows from storage on startup
  - [ ] Spawn FSM actors with persisted state as `InitialState`
  - [ ] Replay unflushed outbox entries
  - [ ] Send `EventResume` to re-establish monitors

- [ ] **Implement cleanup**
  - [ ] Remove FSM actors from tracking when they reach terminal state
  - [ ] Optionally stop actor (if no longer needed)

### For Persistence

- [ ] **Design schema**
  - [ ] State snapshot table with cursor
  - [ ] Event log table (append-only)
  - [ ] Outbox table with dispatch tracking
  - [ ] Event stream table (if pub/sub needed)
  - [ ] Consumer cursor table (if pub/sub needed)

- [ ] **Implement atomic writes**
  - [ ] All writes in single transaction
  - [ ] Upsert state snapshot with new cursor
  - [ ] Append event to log
  - [ ] Insert outbox entries

- [ ] **Implement queries**
  - [ ] `GetState(id)` - load latest state + cursor
  - [ ] `ListPending()` - load all non-terminal workflows
  - [ ] `GetUnflushedOutbox(id)` - load undispatched outbox entries
  - [ ] `GetEventsAfter(cursor, limit)` - for pub/sub consumers

### For Testing

- [ ] **Unit test states**
  - [ ] Test each state's `ProcessEvent` with all possible events
  - [ ] Verify correct next state
  - [ ] Verify emitted outbox events
  - [ ] Test `EventResume` behavior

- [ ] **Integration test with actors**
  - [ ] Spawn ActorSystem, register actors, spawn FSM
  - [ ] Send events, verify state transitions
  - [ ] Verify outbox events dispatched correctly

- [ ] **Test restart safety**
  - [ ] Start workflow, persist state, stop manager
  - [ ] Restart manager, resume workflow
  - [ ] Verify workflow completes successfully

- [ ] **Test idempotency**
  - [ ] Send duplicate events, verify FSM lands in correct state
  - [ ] Send duplicate actor messages, verify no duplicate effects

---

## Anti-Patterns and Pitfalls

### Anti-Pattern: FSM Calls Actors Directly

**Wrong:**
```go
func (s *StateInit) ProcessEvent(...) (...) {
	// ❌ FSM calling actor directly
	env.monitorActor.Tell(ctx, MonitorMsg{...})
	// ...
}
```

**Right:**
```go
func (s *StateInit) ProcessEvent(...) (...) {
	// ✅ FSM emits outbox event
	return &protofsm.StateTransition[...]{
		NextState: nextState,
		NewEvents: fn.Some(protofsm.EmittedEvent[...]{
			Outbox: []OutboxEvent{
				NewOutboxMonitor(...),
			},
		}),
	}, nil
}
```

### Anti-Pattern: Side Effects in ProcessEvent

**Wrong:**
```go
func (s *StateInit) ProcessEvent(...) (...) {
	// ❌ Database write in ProcessEvent
	env.store.UpsertSwap(ctx, s.id, nextState)
	// ...
}
```

**Right:**
```go
func (s *StateInit) ProcessEvent(...) (...) {
	// ✅ Declare persistence via outbox event
	return &protofsm.StateTransition[...]{
		NextState: nextState,
		NewEvents: fn.Some(protofsm.EmittedEvent[...]{
			Outbox: []OutboxEvent{
				NewOutboxPersist(nextState),
			},
		}),
	}, nil
}
```

### Anti-Pattern: Non-Deterministic FSM Logic

**Wrong:**
```go
func (s *StateInit) ProcessEvent(...) (...) {
	// ❌ Non-deterministic time
	now := time.Now()
	// ❌ Non-deterministic randomness
	rand := rand.Int()
	// ❌ External call
	price, _ := http.Get("api/price")
	// ...
}
```

**Right:**
```go
func (s *StateInit) ProcessEvent(...) (...) {
	// ✅ Deterministic via env
	now := env.Clock.Now()
	rand := env.GetRandom()
	price := env.GetCachedPrice()
	// ...
}
```

### Anti-Pattern: Forgetting EventResume

**Wrong:**
```go
func (s *StateMonitoring) ProcessEvent(...) (...) {
	switch event.(type) {
	case EventFunded:
		// Handle funded
	// ❌ No EventResume handler
	}
}
```

**Right:**
```go
func (s *StateMonitoring) ProcessEvent(...) (...) {
	switch event.(type) {
	case EventResume:
		// ✅ Re-emit monitoring outbox event
		return &protofsm.StateTransition[...]{
			NextState: s,
			NewEvents: fn.Some(protofsm.EmittedEvent[...]{
				Outbox: []OutboxEvent{
					NewOutboxMonitor(s.id, env.actorRef),
				},
			}),
		}, nil
	case EventFunded:
		// Handle funded
	}
}
```

### Anti-Pattern: Dispatch Before Persist

**Wrong:**
```go
// ❌ Dispatch outbox events before persisting
for _, evt := range outbox {
	evt.Dispatch(ctx, system)
}

// If crash happens here, events are dispatched but not durable!

store.UpsertState(ctx, state)
```

**Right:**
```go
// ✅ Persist then dispatch
store.UpsertState(ctx, state)

// ONLY AFTER COMMIT:
for _, evt := range outbox {
	evt.Dispatch(ctx, system)
}
```

### Anti-Pattern: Non-Idempotent Actor

**Wrong:**
```go
func (a *CounterActor) Receive(ctx context.Context, msg IncrementMsg) fn.Result[...] {
	// ❌ Not idempotent - receiving twice increments twice
	a.counter++
	return fn.Ok(...)
}
```

**Right:**
```go
func (a *CounterActor) Receive(ctx context.Context, msg IncrementMsg) fn.Result[...] {
	// ✅ Idempotent via dedup
	if a.processed[msg.IdempotencyKey] {
		return fn.Ok(...)
	}

	a.counter++
	a.processed[msg.IdempotencyKey] = true
	return fn.Ok(...)
}
```

---

## Concrete Examples

### Example 1: Simple Document Approval Workflow

**States:** Init → AwaitingReview → Approved/Rejected (terminal)

**Events:**
- `EventSubmitDocument` - starts workflow
- `EventReviewStarted` - reviewer accepted
- `EventApproved` - document approved
- `EventRejected` - document rejected
- `EventResume` - restart safety

**Actors:**
- `ReviewActor` - processes review requests, sends result events back to FSM
- `NotifyActor` - sends notifications

**See:** `baselib/example/example_protofsm.go`

### Example 2: Lightning Loop Out Swap

**States:**
```
Init
  → InitiateSwap
  → AwaitingSwapResp
  → AwaitingHTLCFunded
  → MonitoringHTLC
  → ClaimingOnChain
  → Completed (terminal)
```

**Key Patterns:**
- FSM emits `OutboxMonitorHTLC` (Tell) to start background monitoring
- `HTLCMonitorActor` polls Lightning node, sends `EventHTLCFunded` back to FSM
- FSM emits `OutboxClaimOnChain` (Ask) to claim funds, waits for confirmation
- All state transitions persisted with event cursor for replay safety

**Actors:**
- `HTLCMonitorActor` - monitors Lightning HTLC
- `ChainClaimActor` - broadcasts and confirms on-chain claim tx
- `PersistActor` - persists swap state to database

### Example 3: Multi-Actor Server Out Swap

**States:**
```
Init
  → AwaitingOrder
  → AwaitingHTLC
  → MonitoringHTLC
  → Funded
  → Settled (terminal)
```

**Key Patterns:**
- FSM emits `OutboxInterceptHTLC` to `HTLCInterceptorActor`
- Interceptor spawns goroutine to block and wait for preimage
- FSM emits `OutboxFundSwap` to `FundingActor` to pay on-chain
- FSM emits `OutboxSettleHTLC` with preimage to unblock interceptor
- Dual dispatch: interceptor sends preimage both to blocking channel AND FSM

**Actors:**
- `HTLCInterceptorActor` - intercepts HTLCs, blocks until settled
- `FundingActor` - pays on-chain funding transaction
- `MonitorActor` - monitors blockchain for confirmations
- `PersistActor` - persists swap state

**Complexity Handled:**
- Blocking HTLC + async FSM coordination (dual dispatch pattern)
- Multiple external dependencies (Lightning node, blockchain)
- Restart safety with unflushed outbox replay

---

## Summary

**Core Principles:**

1. **FSMs are pure** - no I/O, deterministic, side-effect free
2. **Outbox events declare side effects** - never execute inline
3. **Actors execute side effects** - idempotent, send results back to FSMs
4. **Persist before notify** - durability before dispatch
5. **Hierarchical composition** - each layer knows only the layer below
6. **Idempotency everywhere** - at-least-once delivery is safe
7. **EventResume for restart** - re-establish pending operations

**Decision Summary:**

- **Env call:** Cheap, repeatable, side-effect free (time, config, cache)
- **Outbox event:** External I/O, state changes, actor calls
- **Tell:** Fire-and-forget, non-blocking, result arrives later
- **Ask:** Request-response, blocking, FSM waits for result
- **Push:** Known consumers, low latency, tight coupling
- **Pub/Sub:** Unknown consumers, eventual consistency, loose coupling

**Build Checklist:**

1. Define sealed types: events, outbox events, states
2. Keep `ProcessEvent` deterministic and pure
3. Specify env with only repeatable helpers
4. Wrap FSM with `protofsm.NewSystemsActorStateMachine`
5. Register service actors with ActorSystem
6. Implement persistence with atomic writes (state + outbox)
7. Dispatch outbox only after commit
8. Make all actors idempotent
9. Implement `EventResume` in non-terminal states
10. Test restart scenarios thoroughly

**When in Doubt:**

- Prefer outbox events over direct calls
- Prefer Tell over Ask (when possible)
- Prefer hybrid (push + pub/sub) for maximum flexibility
- Always test with restarts and duplicate messages

---

**Additional Resources:**

- `baselib/PROTOFSM_ACTOR_GUIDE.md` - Detailed usage guide with step-by-step examples
- `baselib/example/` - Complete document approval workflow example
- `docs/development_guidelines.md` - Code style and testing guidelines
