# Actor-Based State Machine (protofsm + actor) Usage Guide

This guide explains how to build restart-safe, event-driven workflows using the `protofsm` and `actor` packages together. These patterns were refined through refactoring three production swap state machines.

## Table of Contents

1. [Core Concepts](#core-concepts)
2. [Quick Start](#quick-start)
3. [FSM Design Patterns](#fsm-design-patterns)
4. [Actor Integration Patterns](#actor-integration-patterns)
5. [Restart Safety](#restart-safety)
6. [Common Patterns](#common-patterns)
7. [Testing](#testing)
8. [Troubleshooting](#troubleshooting)

---

## Core Concepts

### The protofsm Package

**protofsm** provides a type-safe, event-driven finite state machine:

- **State**: Processes events and returns state transitions
- **Event**: Triggers state transitions (internal to FSM)
- **OutboxEvent**: Emitted to external actors for side effects
- **Environment**: Provides dependencies to state processors
- **StateMachine**: Event queue processor and orchestrator

### The actor Package

**actor** provides message-passing concurrency:

- **Actor**: Goroutine with mailbox, processes messages sequentially
- **ActorRef**: Reference for sending messages (Tell/Ask)
- **ActorSystem**: Manages actor lifecycle and service registry
- **ServiceKey**: Typed key for actor lookup by service name

### Integration: ActorStateMachine

The `protofsm.ActorStateMachine` wraps a `StateMachine` as an `ActorBehavior`, enabling:
- FSM runs inside an actor (one FSM per actor)
- Outbox events automatically dispatch to other actors via service keys
- Multiple FSM actors can coexist in the same ActorSystem

---

## Quick Start

### Step 1: Define Your Events

Use sealed interfaces to define all possible events:

```go
// Event is the sealed interface for all FSM events.
type Event interface {
	isEventSealed()
}

// EventStart begins the workflow.
type EventStart struct {
	ID string
}

func (EventStart) isEventSealed() {}

// EventComplete finishes the workflow.
type EventComplete struct {
	Result string
}

func (EventComplete) isEventSealed() {}

// EventResume is sent when resuming from storage.
type EventResume struct{}

func (EventResume) isEventSealed() {}
```

**Why sealed?** Type safety - only your package can define events.

### Step 2: Define Your Outbox Events

Outbox events are routed to actors for side effects:

```go
// OutboxEvent is the sealed interface for outbox events.
type OutboxEvent interface {
	protofsm.ActorOutboxEvent
	isOutboxEventSealed()
}

// OutboxPersist requests persistence.
type OutboxPersist struct {
	protofsm.RoutedOutboxEvent[StorePersistMsg, StorePersistResp]
}

func NewOutboxPersist(id string, data interface{}) OutboxPersist {
	return OutboxPersist{
		RoutedOutboxEvent: protofsm.NewAskOutboxEvent(
			StoreServiceKey,
			StorePersistMsg{ID: id, Data: data},
		),
	}
}

func (OutboxPersist) isOutboxEventSealed() {}
```

**Key point:** Use `RoutedOutboxEvent` to dispatch to actors via service keys.

### Step 3: Define Your States

Each state implements the `State` interface:

```go
// State is the sealed interface for all FSM states.
type State interface {
	protofsm.State[Event, OutboxEvent, *Environment]
	isStateSealed()
}

// StateInit is the initial state.
type StateInit struct{}

func (StateInit) isStateSealed() {}
func (StateInit) IsTerminal() bool { return false }
func (StateInit) String() string { return "Init" }

func (s *StateInit) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*protofsm.StateTransition[Event, OutboxEvent, *Environment], error) {

	switch e := event.(type) {
	case EventStart:
		nextState := &StateProcessing{id: e.ID}

		return &protofsm.StateTransition[Event, OutboxEvent, *Environment]{
			NextState: nextState,
			NewEvents: fn.Some(protofsm.EmittedEvent[Event, OutboxEvent]{
				Outbox: []OutboxEvent{
					NewOutboxPersist(e.ID, nextState),
				},
			}),
		}, nil

	default:
		return nil, fmt.Errorf("unexpected event %T in StateInit", e)
	}
}
```

**Pattern:** Pattern-match on event type, create next state, emit outbox events.

### Step 4: Define Your Environment

Environment provides dependencies and receives ActorRef injection:

```go
// Environment holds FSM execution context.
type Environment struct {
	store    Store
	actorRef actor.TellOnlyRef[protofsm.ActorMessage[Event]]
}

func NewEnvironment(store Store) *Environment {
	return &Environment{store: store}
}

// SetTellOnlyRef is called by protofsm to inject the FSM's actor reference.
func (e *Environment) SetTellOnlyRef(ref actor.TellOnlyRef[protofsm.ActorMessage[Event]]) {
	e.actorRef = ref
}

func (e *Environment) GetTellOnlyRef() actor.TellOnlyRef[protofsm.ActorMessage[Event]] {
	return e.actorRef
}

// Compile-time check.
var _ protofsm.TellRefEnv[Event] = (*Environment)(nil)
```

**Why?** Actors need to send events back to the FSM (e.g., "operation completed").

### Step 5: Create Actors for Side Effects

Actors handle external operations (storage, API calls, monitoring):

```go
const storeActorName = "store"

var StoreServiceKey = actor.NewServiceKey[StorePersistMsg, StorePersistResp](storeActorName)

type StorePersistMsg struct {
	actor.BaseMessage
	ID   string
	Data interface{}
}

func (m StorePersistMsg) MessageType() string { return "StorePersist" }

type StorePersistResp struct {
	Success bool
}

type StoreActorBehavior struct {
	store Store
}

func (s *StoreActorBehavior) Receive(ctx context.Context,
	msg StorePersistMsg) fn.Result[StorePersistResp] {

	// Perform persistence.
	if err := s.store.Save(msg.ID, msg.Data); err != nil {
		return fn.Err[StorePersistResp](err)
	}

	return fn.Ok(StorePersistResp{Success: true})
}
```

**Pattern:** Actor receives message, performs side effect, returns result.

### Step 6: Wire It All Together

Create ActorSystem, register actors, spawn FSM actors:

```go
func main() {
	ctx := context.Background()

	// Create actor system.
	system := actor.NewActorSystemWithConfig(actor.SystemConfig{
		MailboxCapacity: 100,
	})
	defer system.Shutdown(context.Background())

	// Register shared actors.
	storeActor := &StoreActorBehavior{store: NewStore()}
	actor.RegisterWithSystem(system, storeActorName, StoreServiceKey, storeActor)

	// Create FSM configuration.
	env := NewEnvironment(store)
	cfg := protofsm.StateMachineCfg[Event, OutboxEvent, *Environment]{
		Logger:       logger,
		InitialState: &StateInit{},
		Env:          env,
	}

	// Spawn FSM as actor.
	fsmRef := protofsm.NewSystemsActorStateMachine(
		ctx, cfg, system, "workflow-123",
	)

	// Send initial event.
	fsmRef.Tell(ctx, protofsm.ActorMessage[Event]{
		Event: EventStart{ID: "workflow-123"},
	})
}
```

---

## FSM Design Patterns

### Pattern 1: Sealed Interfaces

Always use sealed interfaces for Events, States, and OutboxEvents:

```go
// Event is sealed via unexported method.
type Event interface {
	isEventSealed()
}

// Only types in this package can implement isEventSealed().
```

**Why?** Type safety - compiler catches invalid event types.

### Pattern 2: State Holds Data, Not Behavior

States are data containers with transition logic:

```go
// GOOD: State holds necessary data for transitions.
type StateProcessing struct {
	id        string
	startTime time.Time
	retries   int
}

// AVOID: State with channels, goroutines, or mutable shared state.
type StateBad struct {
	id        string
	resultCh  chan Result  // ❌ Don't do this
	mu        sync.Mutex   // ❌ States are immutable
}
```

**Why?** States must be serializable for restart safety.

### Pattern 3: Emit Outbox Events for Side Effects

Never perform side effects directly in ProcessEvent:

```go
// GOOD: Emit outbox event for persistence.
func (s *StateInit) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*protofsm.StateTransition[Event, OutboxEvent, *Environment], error) {

	switch e := event.(type) {
	case EventStart:
		nextState := &StateProcessing{id: e.ID}

		return &protofsm.StateTransition[Event, OutboxEvent, *Environment]{
			NextState: nextState,
			NewEvents: fn.Some(protofsm.EmittedEvent[Event, OutboxEvent]{
				Outbox: []OutboxEvent{
					NewOutboxPersist(e.ID, nextState),  // ✅ Emit event
				},
			}),
		}, nil
	}
}

// AVOID: Direct side effects.
func (s *StateBad) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*protofsm.StateTransition[Event, OutboxEvent, *Environment], error) {

	env.store.Save("key", "value")  // ❌ Don't call external systems directly

	return &protofsm.StateTransition[...]{...}, nil
}
```

**Why?** Keeps FSM pure, testable, and allows outbox events to be dispatched asynchronously.

### Pattern 4: EventResume for Restart Safety

Every non-terminal state should handle EventResume:

```go
func (s *StateProcessing) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*protofsm.StateTransition[Event, OutboxEvent, *Environment], error) {

	switch e := event.(type) {
	case EventResume:
		// Re-emit pending outbox events to re-establish monitoring.
		return &protofsm.StateTransition[Event, OutboxEvent, *Environment]{
			NextState: s,  // Stay in same state
			NewEvents: fn.Some(protofsm.EmittedEvent[Event, OutboxEvent]{
				Outbox: []OutboxEvent{
					NewOutboxMonitor(s.id, env.actorRef),
				},
			}),
		}, nil

	// ... other events
	}
}
```

**Why?** When resuming from storage, pending operations must be re-established.

---

## Actor Integration Patterns

### Pattern 1: Actor Sends Events Back to FSM

Actors receive a `TellOnlyRef` to send result events back:

```go
type MonitorMsg struct {
	actor.BaseMessage
	ID           string
	ActorTellRef actor.TellOnlyRef[protofsm.ActorMessage[Event]]
}

func (b *MonitorBehavior) Receive(ctx context.Context,
	msg MonitorMsg) fn.Result[MonitorResp] {

	go func() {
		// Perform background monitoring.
		result := b.pollUntilComplete(ctx, msg.ID)

		// Send result event back to FSM.
		msg.ActorTellRef.Tell(ctx, protofsm.ActorMessage[Event]{
			Event: EventComplete{Result: result},
		})
	}()

	return fn.Ok(MonitorResp{Success: true})
}
```

**Pattern:** Actor starts background work, sends event when done.

### Pattern 2: Tell vs Ask for Outbox Events

Use `Tell` for fire-and-forget, `Ask` for request-response:

```go
// Tell: Fire-and-forget (monitoring, notifications).
func NewOutboxMonitor(id string) OutboxMonitor {
	return OutboxMonitor{
		RoutedOutboxEvent: protofsm.NewTellOutboxEvent(
			MonitorServiceKey,
			MonitorMsg{ID: id},
		),
	}
}

// Ask: Wait for response (persistence, validation).
func NewOutboxPersist(id string) OutboxPersist {
	return OutboxPersist{
		RoutedOutboxEvent: protofsm.NewAskOutboxEvent(
			StoreServiceKey,
			StorePersistMsg{ID: id},
		),
	}
}
```

**Guideline:** Use Ask for critical operations (persistence), Tell for non-blocking operations (monitoring).

### Pattern 3: Service Keys for Routing

Service keys enable location transparency:

```go
// Define service key (global/package-level).
var StoreServiceKey = actor.NewServiceKey[StorePersistMsg, StorePersistResp]("store")

// Register actor.
actor.RegisterWithSystem(system, "store", StoreServiceKey, storeBehavior)

// Emit outbox event (routed automatically via service key).
NewOutboxPersist(id, data)  // Uses StoreServiceKey internally
```

**Why?** FSM doesn't need ActorRef - service key handles routing.

---

## Restart Safety

### Key Principle: Idempotent Resumption

When a process crashes and restarts:
1. Load persisted state from storage
2. Spawn FSM actor with loaded state as `InitialState`
3. Send `EventResume` to re-establish pending operations

### Pattern: Dual Persistence

Persist both state name AND state data:

```go
type StoredWorkflow struct {
	ID        string
	State     State         // The actual state object
	Data      interface{}   // State-specific data
	CreatedAt time.Time
	UpdatedAt time.Time
}

// When loading from DB, reconstruct state from name + data.
func ReconstructStateFromName(stateName string, stored *StoredWorkflow) (State, error) {
	switch stateName {
	case "Processing":
		return &StateProcessing{
			id:        stored.ID,
			startTime: stored.Data.(time.Time),
		}, nil

	// ... other states
	}
}
```

### Pattern: EventResume Re-emits Outbox Events

```go
case EventResume:
	// Re-emit pending operations.
	return &protofsm.StateTransition[Event, OutboxEvent, *Environment]{
		NextState: s,  // Stay in same state
		NewEvents: fn.Some(protofsm.EmittedEvent[Event, OutboxEvent]{
			Outbox: []OutboxEvent{
				// Re-emit monitoring (idempotent).
				NewOutboxMonitor(s.id, env.actorRef),
			},
		}),
	}, nil
```

**Critical:** Actors must be idempotent - receiving the same message twice is safe.

### Pattern: Delta Updates for Persistence

Only persist changed fields:

```go
type WorkflowUpdates struct {
	StartTime *time.Time  // Only set if changed
	Retries   *int        // Only set if changed
	State     State       // Always set to reflect current state
}

// StoreActor applies deltas.
func (s *StoreActor) Receive(ctx context.Context, msg StorePersistMsg) fn.Result[StorePersistResp] {
	existing, _ := s.store.Get(msg.ID)

	// Apply only non-nil fields.
	if msg.Updates.StartTime != nil {
		existing.StartTime = *msg.Updates.StartTime
	}
	if msg.Updates.Retries != nil {
		existing.Retries = *msg.Updates.Retries
	}
	if msg.Updates.State != nil {
		existing.State = msg.Updates.State
	}

	s.store.Save(existing)
	return fn.Ok(StorePersistResp{Success: true})
}
```

---

## Common Patterns

### Pattern: Background Monitoring Actor

Actors can spawn goroutines for long-running operations:

```go
func (b *MonitorBehavior) Receive(ctx context.Context,
	msg MonitorMsg) fn.Result[MonitorResp] {

	// Return immediately (Tell pattern).
	go b.monitorInBackground(ctx, msg)
	return fn.Ok(MonitorResp{Success: true})
}

func (b *MonitorBehavior) monitorInBackground(ctx context.Context, msg MonitorMsg) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if b.checkComplete(msg.ID) {
				// Send completion event to FSM.
				msg.ActorTellRef.Tell(ctx, protofsm.ActorMessage[Event]{
					Event: EventComplete{},
				})
				return
			}
		}
	}
}
```

**Use case:** Polling external systems, waiting for blockchain events, timeouts.

### Pattern: Manager Spawns FSM Actors

One manager owns the ActorSystem and spawns FSM actors:

```go
type Manager struct {
	system     *actor.ActorSystem
	fsmActors  map[string]actor.ActorRef[protofsm.ActorMessage[Event], ...]
	mu         sync.RWMutex
}

func (m *Manager) StartWorkflow(ctx context.Context, id string) error {
	// Spawn FSM actor.
	env := NewEnvironment(m.store)
	cfg := protofsm.StateMachineCfg[Event, OutboxEvent, *Environment]{
		Logger:       m.logger.WithPrefix(id),
		InitialState: &StateInit{},
		Env:          env,
	}

	fsmRef := protofsm.NewSystemsActorStateMachine(ctx, cfg, m.system, id)

	m.mu.Lock()
	m.fsmActors[id] = fsmRef
	m.mu.Unlock()

	// Send initial event.
	fsmRef.Tell(ctx, protofsm.ActorMessage[Event]{
		Event: EventStart{ID: id},
	})

	return nil
}

func (m *Manager) ResumeWorkflows(ctx context.Context) error {
	workflows, _ := m.store.ListPending()

	for _, wf := range workflows {
		// Spawn FSM actor with persisted state.
		cfg := protofsm.StateMachineCfg[Event, OutboxEvent, *Environment]{
			Logger:       m.logger.WithPrefix(wf.ID),
			InitialState: wf.State,  // Reconstructed from storage
			Env:          NewEnvironment(m.store),
		}

		fsmRef := protofsm.NewSystemsActorStateMachine(ctx, cfg, m.system, wf.ID)

		m.mu.Lock()
		m.fsmActors[wf.ID] = fsmRef
		m.mu.Unlock()

		// Re-establish pending operations.
		fsmRef.Tell(ctx, protofsm.ActorMessage[Event]{
			Event: EventResume{},
		})
	}

	return nil
}
```

**Pattern:** Manager = one ActorSystem, multiple FSM actors (one per workflow instance).

### Pattern: Cleanup on Terminal States

Remove FSM actors when workflow completes:

```go
type Environment struct {
	store       Store
	actorRef    actor.TellOnlyRef[protofsm.ActorMessage[Event]]
	cleanupFunc func(ctx context.Context, id string)
}

// In terminal states, call cleanup.
func (s *StateCompleted) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*protofsm.StateTransition[Event, OutboxEvent, *Environment], error) {

	// Cleanup FSM actor from manager registry.
	if env.cleanupFunc != nil {
		env.cleanupFunc(ctx, s.id)
	}

	return nil, fmt.Errorf("no events expected in terminal state")
}
```

### Pattern: Dual Dispatch for Blocking Operations

When you have both async actors and blocking operations (e.g., HTLC interception):

```go
// Actor sends to BOTH:
// 1. Blocking channel (for waiting goroutine)
// 2. FSM event (for state machine completion)

func (h *HTLCInterceptor) Receive(ctx context.Context,
	msg SettleMsg) fn.Result[SettleResp] {

	// Send preimage to blocking channel.
	h.claimChans[msg.PaymentHash] <- msg.Preimage

	// Send event to FSM.
	msg.ActorTellRef.Tell(ctx, protofsm.ActorMessage[Event]{
		Event: EventHTLCSettled{},
	})

	return fn.Ok(SettleResp{Success: true})
}
```

---

## Testing

### Unit Testing States

Test state transitions in isolation:

```go
func TestStateInit_EventStart(t *testing.T) {
	state := &StateInit{}
	env := &Environment{}

	transition, err := state.ProcessEvent(
		context.Background(),
		EventStart{ID: "test-123"},
		env,
	)

	require.NoError(t, err)
	require.Equal(t, "Processing", transition.NextState.String())
	require.Len(t, transition.NewEvents.UnwrapOr(...).Outbox, 1)
}
```

### Integration Testing with Actors

Test the full actor + FSM integration:

```go
func TestWorkflowIntegration(t *testing.T) {
	ctx := context.Background()
	system := actor.NewActorSystemWithConfig(actor.SystemConfig{})
	defer system.Shutdown(context.Background())

	// Register actors.
	actor.RegisterWithSystem(system, "store", StoreServiceKey, &StoreActorBehavior{})

	// Spawn FSM.
	cfg := protofsm.StateMachineCfg[Event, OutboxEvent, *Environment]{
		Logger:       btclog.Disabled,
		InitialState: &StateInit{},
		Env:          NewEnvironment(store),
	}

	fsmRef := protofsm.NewSystemsActorStateMachine(ctx, cfg, system, "test-workflow")

	// Send event and verify state transition.
	resp := fsmRef.Ask(ctx, protofsm.ActorMessage[Event]{
		Event: EventStart{ID: "test-123"},
	}).Await(ctx)

	require.NoError(t, resp.Err())
}
```

### Testing Restart Safety

Test that workflows resume correctly:

```go
func TestRestart(t *testing.T) {
	// 1. Start workflow, persist state.
	manager1 := NewManager(store)
	manager1.StartWorkflow(ctx, "wf-1")
	// ... wait for state = Processing

	// 2. Stop manager (simulate crash).
	manager1.Stop()

	// 3. Restart with same store.
	manager2 := NewManager(store)
	manager2.ResumeWorkflows(ctx)

	// 4. Verify workflow continues from persisted state.
	// ... assert workflow completes successfully
}
```

---

## Troubleshooting

### Issue: "swap not found" when persisting

**Cause:** StoreActor calls UpdateSwap before swap exists in store.

**Solution:** Check if swap exists, use CreateSwap for new, UpdateSwap for existing:

```go
existing, err := s.store.Get(msg.ID)
isNew := err != nil

if isNew {
	s.store.Create(newSwap)
} else {
	s.store.Update(existing)
}
```

### Issue: FSM stuck in non-terminal state

**Cause:** Actor performed operation but didn't send result event to FSM.

**Solution:** Always send result event via `ActorTellRef`:

```go
// After completing operation:
msg.ActorTellRef.Tell(ctx, protofsm.ActorMessage[Event]{
	Event: EventOperationComplete{},
})
```

### Issue: EventResume doesn't re-establish monitoring

**Cause:** State doesn't re-emit outbox events on EventResume.

**Solution:** Handle EventResume in every non-terminal state:

```go
case EventResume:
	return &protofsm.StateTransition[...]{
		NextState: s,
		NewEvents: fn.Some(protofsm.EmittedEvent[...]{
			Outbox: []OutboxEvent{
				NewOutboxMonitor(s.id, env.actorRef),
			},
		}),
	}, nil
```

### Issue: Method name conflicts in composed interfaces

**Cause:** Multiple interfaces with same method name but different return types.

**Solution:** Use package-specific method names:

```go
// Instead of:
type InStore interface {
	UpsertSwap(ctx context.Context, swap *StoredSwap) error
}

type OutStore interface {
	UpsertSwap(ctx context.Context, swap *StoredSwap) error  // Conflict!
}

// Do:
type InStore interface {
	UpsertInSwap(ctx context.Context, swap *StoredSwap) error
}

type OutStore interface {
	UpsertOutSwap(ctx context.Context, swap *StoredSwap) error
}
```

---

## Best Practices Summary

1. ✅ **Use sealed interfaces** for Events, States, OutboxEvents
2. ✅ **States are immutable data** - no channels, mutexes, or goroutines
3. ✅ **Emit outbox events for side effects** - never call external systems from ProcessEvent
4. ✅ **Always handle EventResume** in non-terminal states
5. ✅ **Actors send result events** back to FSM via ActorTellRef
6. ✅ **Use Tell for async, Ask for sync** outbox events
7. ✅ **One ActorSystem per manager**, multiple FSM actors per workflow instance
8. ✅ **Persist state after every transition** via OutboxPersist
9. ✅ **Make actors idempotent** - safe to receive same message twice
10. ✅ **Test restart scenarios** - load from storage, send EventResume, verify completion

---

## Example: See It In Action

Check `baselib/example/` for a complete document approval workflow:
- `example_protofsm.go` - FSM states and events
- `example_actors.go` - ReviewService and NotificationService actors
- `example_test.go` - Integration test with actor system

Or study the production implementations:
- `sdk/swaps/in/` - Client in-swap (simple: fund → monitor → complete/refund)
- `swapserver/out/` - Server out-swap (complex: intercept → fund → monitor → settle HTLC)
- `swapserver/in/` - Server in-swap (multi-actor: monitor → pay invoice → claim)

---

**Summary:** The actor + protofsm pattern provides restart-safe, event-driven workflows with clean separation between FSM logic (pure state transitions) and side effects (actors). Key insight: FSMs emit outbox events which are automatically routed to actors, actors send result events back to FSMs.
