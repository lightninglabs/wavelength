# Layered Testing Guide for LLM-Assisted Development

This guide describes a layered verification approach for developing features with
LLM assistance. The goal is to reduce review burden while maintaining high
confidence in correctness.

## The Problem

When using LLMs to generate significant amounts of code, traditional line-by-line
review becomes impractical. Yet shipping unreviewed code is risky. We need a
verification strategy that:

1. Makes the **intent** explicit and reviewable (specs are easier to review than
   implementations)
2. Catches bugs through **multiple independent layers** (defense in depth)
3. Leverages the strengths of both humans (intent, edge cases, security) and
   machines (exhaustive checking, consistency)

## The Solution: Layered Verification

```
┌─────────────────────────────────────────────────────────────────────┐
│ Layer 1: BDD/Gherkin Specs                                          │
│   - Behavior specifications in plain English                        │
│   - Reviewable by humans (and generatable by AI)                    │
│   - Executable acceptance criteria                                  │
└─────────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────────┐
│ Layer 2: Property-Based Tests                                       │
│   - Invariants that must ALWAYS hold                                │
│   - Catches edge cases specs miss                                   │
│   - Uses pgregory.net/rapid                                         │
└─────────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────────┐
│ Layer 3: Type System + Linting                                      │
│   - Compile-time guarantees via sealed interfaces                   │
│   - Style enforcement via ast-grep rules                            │
│   - Automated, zero-effort gate                                     │
└─────────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────────┐
│ Layer 4: TransitionTable Documentation                              │
│   - Compile-time FSM contract                                       │
│   - Auto-generates documentation                                    │
│   - Can validate against Gherkin scenarios                          │
└─────────────────────────────────────────────────────────────────────┘
```

The key insight: **if all layers pass, you can have high confidence in
correctness without reading every line of implementation code.**

---

## Layer 1: BDD/Gherkin Specifications

### What Is BDD?

Behavior-Driven Development (BDD) uses plain-English specifications to describe
system behavior. Gherkin is the structured format:

```gherkin
Feature: Timeout scheduling
  As a system component
  I want to schedule timeouts
  So that I can handle time-based events reliably

  Scenario: Schedule a new timeout
    Given a timeout actor with no active timeouts
    When I schedule a timeout "payment-deadline" for 5 seconds
    Then the timeout should be registered
    And after 5 seconds the callback should receive an expiry notification

  Scenario: Cancel an existing timeout
    Given a timeout actor with an active timeout "payment-deadline"
    When I cancel the timeout "payment-deadline"
    Then the timeout should be removed
    And the callback should NOT receive an expiry notification

  Scenario: Reschedule an existing timeout
    Given a timeout actor with an active timeout "payment-deadline" for 10 seconds
    When I schedule a timeout "payment-deadline" for 5 seconds
    Then the original timeout should be cancelled
    And a new timeout should be registered for 5 seconds
```

### Why Gherkin Works Well with Actors

Actors have a natural Given/When/Then structure:

| Gherkin      | Actor Concept                        |
|--------------|--------------------------------------|
| **Given**    | Actor state / test harness setup     |
| **When**     | Message sent to actor (Tell or Ask)  |
| **Then**     | Response received / outbox emitted   |

### Why Gherkin Works Well with Protofsm

FSM transitions map directly to scenarios:

| Gherkin      | FSM Concept                          |
|--------------|--------------------------------------|
| **Given**    | Current state                        |
| **When**     | Event received                       |
| **Then**     | New state + outbox events            |

### Writing Effective Gherkin Specs

**DO specify:**
- Happy path scenarios
- Key error conditions
- State transitions and their triggers
- Observable outputs (responses, outbox events)

**DON'T specify:**
- Implementation details (internal data structures)
- Performance characteristics (use benchmarks)
- Every possible edge case (use property tests)

### Gherkin File Organization

```
features/
├── timeout/
│   ├── scheduling.feature      # Core scheduling behavior
│   └── cancellation.feature    # Cancellation flows
├── rounds/
│   ├── registration.feature    # Client registration flow
│   ├── signing.feature         # Signing coordination
│   └── confirmation.feature    # Round confirmation
└── vtxo/
    ├── lifecycle.feature       # VTXO state transitions
    └── spending.feature        # Spending flows
```

### Step Definition Patterns

Step definitions connect Gherkin to Go code. Use these patterns:

```go
// features/timeout/timeout_steps_test.go
package timeout_test

import (
	"context"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/lightninglabs/darepo/timeout"
)

type timeoutTestContext struct {
	t        *testing.T
	actor    *timeout.Actor
	callback *mockCallback
	lastErr  error
}

func (tc *timeoutTestContext) aTimeoutActorWithNoActiveTimeouts() error {
	tc.actor = timeout.NewActor()
	tc.callback = newMockCallback()
	return nil
}

func (tc *timeoutTestContext) iScheduleATimeoutForSeconds(
	id string, seconds int) error {

	ctx := context.Background()
	req := &timeout.ScheduleTimeoutRequest{
		ID:       timeout.ID(id),
		Duration: time.Duration(seconds) * time.Second,
		Callback: tc.callback,
	}

	result := tc.actor.Receive(ctx, req)
	if result.IsErr() {
		tc.lastErr = result.Err()
	}
	return nil
}

func (tc *timeoutTestContext) theTimeoutShouldBeRegistered() error {
	// Query actor state or check response
	if tc.lastErr != nil {
		return fmt.Errorf("timeout registration failed: %w", tc.lastErr)
	}
	return nil
}

func (tc *timeoutTestContext) afterSecondsTheCallbackShouldReceive(
	seconds int) error {

	msg, ok := tc.callback.waitForMessage(
		time.Duration(seconds)*time.Second + 100*time.Millisecond,
	)
	if !ok {
		return fmt.Errorf("callback did not receive message within %ds",
			seconds)
	}
	if msg.ID != tc.expectedID {
		return fmt.Errorf("wrong timeout ID: got %s, want %s",
			msg.ID, tc.expectedID)
	}
	return nil
}

func InitializeScenario(ctx *godog.ScenarioContext) {
	tc := &timeoutTestContext{}

	ctx.Given(`^a timeout actor with no active timeouts$`,
		tc.aTimeoutActorWithNoActiveTimeouts)
	ctx.When(`^I schedule a timeout "([^"]*)" for (\d+) seconds$`,
		tc.iScheduleATimeoutForSeconds)
	ctx.Then(`^the timeout should be registered$`,
		tc.theTimeoutShouldBeRegistered)
	ctx.Then(`^after (\d+) seconds the callback should receive`,
		tc.afterSecondsTheCallbackShouldReceive)
}
```

---

## Layer 2: Property-Based Tests

### What Are Property Tests?

Property tests verify **invariants** that must hold across all possible inputs.
Instead of testing specific examples, you define properties and let the test
framework generate thousands of random inputs.

### When to Use Property Tests

Use property tests for:
- **Invariants**: "Balance can never go negative"
- **Roundtrip properties**: "Encode then decode returns original"
- **Algebraic properties**: "Order of independent operations doesn't matter"
- **State machine properties**: "All reachable states are valid"

### Property Test Patterns for Actors

```go
func TestActorMessageOrdering(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random sequence of messages.
		messages := rapid.SliceOf(genValidMessage()).Draw(t, "messages")

		actor := NewActor()
		var responses []Response

		// Send all messages.
		for _, msg := range messages {
			resp := actor.Receive(context.Background(), msg)
			if resp.IsOk() {
				responses = append(responses, resp.Unwrap())
			}
		}

		// INVARIANT: Response count <= message count.
		if len(responses) > len(messages) {
			t.Fatalf("more responses than messages")
		}

		// INVARIANT: All responses reference valid message IDs.
		msgIDs := make(map[string]bool)
		for _, msg := range messages {
			msgIDs[msg.ID()] = true
		}
		for _, resp := range responses {
			if !msgIDs[resp.ForMessageID()] {
				t.Fatalf("response references unknown message: %s",
					resp.ForMessageID())
			}
		}
	})
}
```

### Property Test Patterns for FSMs

```go
func TestFSMStateInvariants(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random sequence of valid events.
		events := rapid.SliceOf(genValidEvent()).Draw(t, "events")

		env := newTestEnvironment()
		state := &CreatedState{}

		// Apply all events.
		for _, event := range events {
			transition, err := state.ProcessEvent(
				context.Background(), event, env,
			)
			if err != nil {
				// Some events may be invalid in certain states.
				continue
			}
			state = transition.NextState

			// INVARIANT: State is never nil.
			if state == nil {
				t.Fatal("state became nil")
			}

			// INVARIANT: Terminal states stay terminal.
			if state.IsTerminal() {
				for _, followup := range events {
					trans2, _ := state.ProcessEvent(
						context.Background(), followup, env,
					)
					if trans2 != nil && trans2.NextState != state {
						t.Fatal("terminal state changed")
					}
				}
				break
			}
		}
	})
}

func TestFSMTransitionDeterminism(t *rapid.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Same state + same event = same result.
		state := genState().Draw(t, "state")
		event := genEvent().Draw(t, "event")
		env := newTestEnvironment()

		trans1, err1 := state.ProcessEvent(context.Background(), event, env)
		trans2, err2 := state.ProcessEvent(context.Background(), event, env)

		if (err1 == nil) != (err2 == nil) {
			t.Fatal("non-deterministic error behavior")
		}
		if err1 == nil {
			if reflect.TypeOf(trans1.NextState) !=
				reflect.TypeOf(trans2.NextState) {
				t.Fatal("non-deterministic state transition")
			}
		}
	})
}
```

### Common Properties to Test

| Property | Description | Example |
|----------|-------------|---------|
| **Idempotence** | Applying same operation twice = once | Cancelling already-cancelled timeout |
| **Commutativity** | Order doesn't matter | Independent registrations |
| **Roundtrip** | Encode/decode preserves data | Serializing FSM state |
| **Monotonicity** | Values only increase/decrease | Block height never decreases |
| **Boundedness** | Values stay within limits | Balance >= 0 |
| **Reachability** | All states reachable | FSM has no dead states |
| **Liveness** | Terminal states eventually reached | Rounds eventually confirm or fail |

---

## Layer 3: Type System and Linting

### Sealed Interfaces

The codebase uses sealed interfaces to ensure type safety at compile time:

```go
// Event is a sealed interface - only types in this package can implement it.
type Event interface {
	eventSealed()  // Unexported method seals the interface
}

// State is a sealed interface for FSM states.
type State interface {
	protofsm.State[Event, OutboxEvent, *Environment]
	stateSealed()
}
```

This means:
- The compiler verifies exhaustive switch statements
- New states/events require explicit handling
- Impossible states are unrepresentable

### Linting Gates

The following must pass before code is accepted:

```bash
make lint        # Go linters (staticcheck, errcheck, etc.)
make ast-lint    # AST-level style rules
```

Key rules enforced:
- All functions have comments
- Proper code spacing and formatting
- No inline comments
- Symmetric wrapping for multi-line calls
- Structured logging format

---

## Layer 4: TransitionTable Documentation

### What Is TransitionTable?

TransitionTable is a compile-time structure that documents all valid FSM
transitions. It serves as:

1. **Documentation**: Human-readable FSM specification
2. **Contract**: Machine-verifiable behavior definition
3. **Test oracle**: Expected transitions can be verified against actual behavior

### Defining a TransitionTable

```go
var RoundFSMTransitions = protofsm.TransitionTable[State, Event, OutboxEvent]{
	MachineName: "RoundFSM",
	States: []protofsm.StateTransitions[State, Event, OutboxEvent]{
		{
			FromState: &CreatedState{},
			Transitions: []protofsm.TransitionEntry[State, Event, OutboxEvent]{
				{
					Event:       &ClientJoinRequestEvent{},
					ToState:     &RegistrationState{},
					Description: "First client joins, transition to registration",
					EmitsOutbox: []OutboxEvent{
						&ClientSuccessResp{},
						&ScheduleTimeoutOutbox{},
					},
				},
			},
		},
		{
			FromState: &RegistrationState{},
			Transitions: []protofsm.TransitionEntry[State, Event, OutboxEvent]{
				{
					Event:       &ClientJoinRequestEvent{},
					ToState:     &RegistrationState{},
					Description: "Additional client joins",
					EmitsOutbox: []OutboxEvent{&ClientSuccessResp{}},
				},
				{
					Event:       &SealEvent{},
					ToState:     &SigningState{},
					Description: "Registration closes, begin signing",
					EmitsOutbox: []OutboxEvent{&RequestSignaturesOutbox{}},
				},
			},
		},
		// ... more states
	},
}
```

### Generating Documentation

```go
func TestGenerateTransitionDocs(t *testing.T) {
	markdown := RoundFSMTransitions.RenderMarkdown()
	// Write to docs/ or verify against existing docs.
}
```

Output:
```markdown
# RoundFSM State Transitions

| From State | Event | To State | Outbox Messages | Description |
|------------|-------|----------|-----------------|-------------|
| CreatedState | ClientJoinRequestEvent | RegistrationState | ClientSuccessResp, ScheduleTimeoutOutbox | First client joins |
| RegistrationState | ClientJoinRequestEvent | RegistrationState | ClientSuccessResp | Additional client joins |
| RegistrationState | SealEvent | SigningState | RequestSignaturesOutbox | Registration closes |
```

### Validating Against Gherkin

TransitionTable can be used to auto-generate or validate Gherkin scenarios:

```go
func TestTransitionTableMatchesGherkin(t *testing.T) {
	for _, st := range RoundFSMTransitions.States {
		for _, trans := range st.Transitions {
			// Verify corresponding Gherkin scenario exists.
			scenarioName := fmt.Sprintf("%s on %s",
				reflect.TypeOf(trans.Event).Elem().Name(),
				reflect.TypeOf(st.FromState).Elem().Name(),
			)
			if !gherkinScenarioExists(scenarioName) {
				t.Errorf("missing Gherkin scenario: %s", scenarioName)
			}
		}
	}
}
```

---

## Feature Implementation Workflow

### Step 1: Write the Specification

Start with Gherkin scenarios that describe the desired behavior:

```gherkin
Feature: VTXO spending via out-of-round transfer

  Scenario: Successful OOR transfer initiation
    Given a VTXO actor with a confirmed VTXO worth 100000 sats
    When I request an OOR transfer of 50000 sats to "recipient-pubkey"
    Then the VTXO should transition to "pending-oor" state
    And an OOR session should be initiated with the server
    And the remaining balance should be 50000 sats minus fees

  Scenario: OOR transfer with insufficient balance
    Given a VTXO actor with a confirmed VTXO worth 100000 sats
    When I request an OOR transfer of 150000 sats to "recipient-pubkey"
    Then the request should fail with "insufficient balance"
    And the VTXO should remain in "confirmed" state
```

### Step 2: Define Property Invariants

Identify invariants that must always hold:

```go
// Properties for VTXO OOR transfers:
// 1. Total value is conserved (input = outputs + fees)
// 2. VTXO state transitions are valid per TransitionTable
// 3. No negative balances
// 4. OOR session IDs are unique
// 5. Failed transfers don't modify VTXO state
```

### Step 3: Define the TransitionTable

Document expected FSM behavior:

```go
var VTXOTransitions = protofsm.TransitionTable[VTXOState, VTXOEvent, VTXOOutbox]{
	MachineName: "VTXO",
	States: []protofsm.StateTransitions[...]{
		{
			FromState: &ConfirmedState{},
			Transitions: []protofsm.TransitionEntry[...]{
				{
					Event:       &InitiateOOREvent{},
					ToState:     &PendingOORState{},
					Description: "Begin OOR transfer",
					EmitsOutbox: []VTXOOutbox{&StartOORSessionOutbox{}},
				},
				// ... error cases
			},
		},
	},
}
```

### Step 4: Generate Implementation

With specs, properties, and TransitionTable defined, the implementation has
clear contracts to satisfy. LLM-generated code must:

1. Pass all Gherkin scenarios
2. Satisfy all property tests
3. Match TransitionTable transitions
4. Pass type checking and linting

### Step 5: Verification

Run the full verification suite:

```bash
# Layer 1: BDD specs
make bdd pkg=vtxo

# Layer 2: Property tests
make unit pkg=vtxo

# Layer 3: Type system + linting
make lint
make ast-lint

# Layer 4: TransitionTable validation
make unit pkg=vtxo case=TestTransitionTable
```

### Step 6: Spot-Check Review

With all layers passing, review focuses on:

1. **Spec review**: Do the Gherkin scenarios capture the right behavior?
2. **Property review**: Are the invariants complete?
3. **Security review**: Any obvious vulnerabilities? (injection, overflow, etc.)
4. **Spot-check implementation**: Random sampling of generated code

---

## Templates for LLM Code Generation

### Actor Behavior Template

```go
// [ActorName]Actor handles [description of responsibility].
type [ActorName]Actor struct {
	cfg   *[ActorName]Config
	state [ActorName]State
	env   *[ActorName]Environment
}

// Receive processes incoming messages and returns responses.
// This method implements actor.ActorBehavior.
func (a *[ActorName]Actor) Receive(ctx context.Context,
	msg [ActorName]Msg) fn.Result[[ActorName]Resp] {

	switch m := msg.(type) {
	case *[MessageType1]:
		return a.handle[MessageType1](ctx, m)

	case *[MessageType2]:
		return a.handle[MessageType2](ctx, m)

	default:
		return fn.Err[[ActorName]Resp](
			fmt.Errorf("unknown message type: %T", msg),
		)
	}
}

// handle[MessageType1] processes [description].
func (a *[ActorName]Actor) handle[MessageType1](ctx context.Context,
	msg *[MessageType1]) fn.Result[[ActorName]Resp] {

	// 1. Validate input.
	if err := validate[MessageType1](msg); err != nil {
		return fn.Err[[ActorName]Resp](err)
	}

	// 2. Perform operation.
	result, err := a.env.DoSomething(ctx, msg.Field)
	if err != nil {
		return fn.Err[[ActorName]Resp](err)
	}

	// 3. Update state if needed.
	a.state = a.state.With[Update](result)

	// 4. Return success response.
	return fn.Ok[[ActorName]Resp](&[ResponseType]{
		Success: true,
		Data:    result,
	})
}
```

### FSM State Template

```go
// [StateName]State represents [description of this state].
// This state is reached when [conditions] and transitions to
// [next states] when [events occur].
type [StateName]State struct {
	// [Field1] holds [description].
	[Field1] [Type1]

	// [Field2] holds [description].
	[Field2] [Type2]
}

// stateSealed implements the sealed State interface.
func (s *[StateName]State) stateSealed() {}

// IsTerminal returns whether this is a terminal state.
func (s *[StateName]State) IsTerminal() bool {
	return [true/false]
}

// String returns a human-readable state name.
func (s *[StateName]State) String() string {
	return "[StateName]State"
}

// ProcessEvent handles incoming events and returns state transitions.
func (s *[StateName]State) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	switch e := event.(type) {
	case *[EventType1]:
		return s.handle[EventType1](ctx, e, env)

	case *[EventType2]:
		return s.handle[EventType2](ctx, e, env)

	case *EventResume:
		// Re-establish monitoring after restart.
		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					// Re-emit pending operations.
				},
			}),
		}, nil

	default:
		return nil, fmt.Errorf(
			"unexpected event %T in [StateName]State", event,
		)
	}
}

// handle[EventType1] processes [description].
func (s *[StateName]State) handle[EventType1](ctx context.Context,
	e *[EventType1], env *Environment) (*StateTransition, error) {

	// 1. Validate event.
	if err := e.Validate(); err != nil {
		return errorTransition(s, err), nil
	}

	// 2. Perform any environment interactions.
	result, err := env.DoSomething(ctx, e.Field)
	if err != nil {
		return errorTransition(s, err), nil
	}

	// 3. Create next state (immutable - never modify s).
	nextState := &[NextStateName]State{
		Field1: s.Field1,
		Field2: result,
	}

	// 4. Return transition with outbox events.
	return &StateTransition{
		NextState: nextState,
		NewEvents: fn.Some(EmittedEvent{
			Outbox: []OutboxEvent{
				&[OutboxEventType]{Data: result},
			},
		}),
	}, nil
}
```

### Gherkin Step Definition Template

```go
package [feature]_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/cucumber/godog"
)

// [feature]TestContext holds test state across steps.
type [feature]TestContext struct {
	t       *testing.T
	actor   *[Actor]
	fsm     *[FSM]
	lastErr error
	// Add fields for tracking state between steps.
}

// Step: Given [description]
func (tc *[feature]TestContext) [givenStepName]([params]) error {
	// Set up preconditions.
	tc.actor = New[Actor]()
	return nil
}

// Step: When [description]
func (tc *[feature]TestContext) [whenStepName]([params]) error {
	// Perform the action.
	ctx := context.Background()
	result := tc.actor.Receive(ctx, &[Message]{Field: [param]})
	if result.IsErr() {
		tc.lastErr = result.Err()
	}
	return nil
}

// Step: Then [description]
func (tc *[feature]TestContext) [thenStepName]([params]) error {
	// Verify the outcome.
	if tc.lastErr != nil {
		return fmt.Errorf("unexpected error: %w", tc.lastErr)
	}
	// Add specific assertions.
	return nil
}

// InitializeScenario registers step definitions.
func InitializeScenario(ctx *godog.ScenarioContext) {
	tc := &[feature]TestContext{}

	ctx.Given(`^[regex pattern]$`, tc.[givenStepName])
	ctx.When(`^[regex pattern]$`, tc.[whenStepName])
	ctx.Then(`^[regex pattern]$`, tc.[thenStepName])
}
```

### Property Test Template

```go
func Test[Feature]Properties(t *testing.T) {
	t.Run("invariant: [description]", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			// Generate random inputs.
			input := gen[Input]().Draw(t, "input")

			// Perform operation.
			result := doOperation(input)

			// Assert invariant.
			if ![invariantCondition](result) {
				t.Fatalf("invariant violated: [description]")
			}
		})
	})

	t.Run("property: [description]", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			// Generate random inputs.
			a := gen[TypeA]().Draw(t, "a")
			b := gen[TypeB]().Draw(t, "b")

			// Assert property.
			resultAB := combine(a, b)
			resultBA := combine(b, a)
			if resultAB != resultBA {
				t.Fatalf("commutativity violated")
			}
		})
	})
}

// gen[Type] returns a rapid generator for [Type].
func gen[Type]() *rapid.Generator[[Type]] {
	return rapid.Custom(func(t *rapid.T) [Type] {
		return [Type]{
			Field1: rapid.String().Draw(t, "field1"),
			Field2: rapid.IntRange(0, 1000).Draw(t, "field2"),
		}
	})
}
```

---

## Makefile Integration

Add these targets to enable the layered testing workflow:

```makefile
# Run BDD tests for a specific package.
bdd:
ifdef pkg
	cd $(pkg) && godog run --format pretty
else
	@echo "Usage: make bdd pkg=<package>"
endif

# Run all BDD tests.
bdd-all:
	find . -name "*.feature" -exec dirname {} \; | sort -u | \
		xargs -I{} sh -c 'cd {} && godog run --format pretty'

# Run property tests (subset of unit tests using rapid).
property:
ifdef pkg
	go test -v -run "Property" ./$(pkg)/...
else
	go test -v -run "Property" ./...
endif

# Full verification suite.
verify: lint ast-lint bdd-all unit
	@echo "All verification layers passed"
```

---

## When to Use Each Layer

| Scenario | Layer 1 (BDD) | Layer 2 (Property) | Layer 3 (Types) | Layer 4 (TransitionTable) |
|----------|---------------|-------------------|-----------------|---------------------------|
| New actor | Yes | Yes (message ordering) | Yes (sealed messages) | N/A |
| New FSM | Yes | Yes (state invariants) | Yes (sealed states/events) | Yes |
| New message type | Maybe | Maybe | Yes | N/A |
| Bug fix | Yes (regression) | Maybe | Yes | Maybe |
| Refactoring | No (behavior unchanged) | Yes (verify invariants) | Yes | No |
| Performance | No | No | Yes | No |

---

## Common Pitfalls

### BDD Pitfalls

1. **Over-specifying implementation details**
   - BAD: "Then the internal map should have 3 entries"
   - GOOD: "Then there should be 3 registered clients"

2. **Testing every edge case in Gherkin**
   - Use property tests for edge cases
   - BDD is for documenting key behaviors

3. **Flaky time-based scenarios**
   - Use mock clocks or generous timeouts
   - Avoid "wait exactly 5 seconds"

### Property Test Pitfalls

1. **Generators that produce invalid inputs**
   - Constrain generators to valid domain
   - Use `rapid.Filter()` sparingly (prefer constrained generation)

2. **Properties that are too weak**
   - "Result is not nil" is too weak
   - Define meaningful invariants

3. **Slow generators**
   - Property tests run thousands of iterations
   - Keep generators fast

### FSM Pitfalls

1. **Mutable state**
   - States must be immutable
   - Always create new state objects

2. **Side effects in ProcessEvent**
   - ProcessEvent should be pure
   - Side effects go in outbox events

3. **Missing EventResume handler**
   - All non-terminal states need EventResume
   - Enables restart recovery

---

## Summary

The layered testing approach provides confidence in LLM-generated code through:

1. **BDD Specs**: Reviewable behavior contracts
2. **Property Tests**: Exhaustive invariant checking
3. **Type System**: Compile-time guarantees
4. **TransitionTable**: FSM documentation and validation

This enables a workflow where:
- Humans review specs and properties (intent)
- Machines verify implementation (correctness)
- Review burden is reduced while confidence remains high

The actor system and protofsm patterns are particularly well-suited to this
approach because they enforce clear boundaries between intent (messages, events)
and implementation (handlers, transitions).
