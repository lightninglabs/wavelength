package baselib_test

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
)

// ExampleActorStateMachine demonstrates integrating a state machine with
// actor services. The FSM emits outbox events that are routed to the
// ReviewService and NotificationService actors.
func ExampleActorStateMachine() {
	ctx := context.Background()
	reviewStart := make(chan struct{}, 1)

	// Create actor system.
	system := actor.NewActorSystemWithConfig(actor.SystemConfig{
		MailboxCapacity: 10,
	})
	defer func() {
		_ = system.Shutdown(context.Background())
	}()

	// Register ReviewService actor.
	reviewBehavior := &ReviewServiceBehavior{
		startProcessing: reviewStart,
	}
	actor.RegisterWithSystem(
		system, "review-service", ReviewServiceKey, reviewBehavior,
	)

	// Register NotificationService actor.
	notifyBehavior := &NotificationServiceBehavior{}
	actor.RegisterWithSystem(
		system, "notify-service", NotifyServiceKey, notifyBehavior,
	)

	// Create FSM environment.
	env := &DocEnvironment{}

	// Create FSM config.
	cfg := protofsm.StateMachineCfg[
		DocEvent,
		DocOutboxEvent,
		*DocEnvironment,
	]{
		Logger:       btclog.Disabled,
		InitialState: &StateInit{},
		Env:          env,
	}

	// Spawn FSM as actor.
	fsmRef := protofsm.NewSystemsActorStateMachine(
		ctx, cfg, system, "document-workflow",
	)

	// Submit a document for review.
	fmt.Println("=== Submitting Document ===")
	resp1 := fsmRef.Ask(
		ctx, protofsm.ActorMessage[DocEvent]{
			Event: EventSubmitDocument{
				DocumentID: "DOC-123",
				Author:     "Alice",
			},
		},
	).Await(ctx)

	if resp1.IsErr() {
		// Release the review worker in error paths so shutdown is
		// clean.
		reviewStart <- struct{}{}
		fmt.Printf("Error: %v\n", resp1.Err())

		return
	}

	// Give async actors time to process the full workflow:
	// 1. ReviewService receives request
	// 2. ReviewService sends EventReviewStarted back to FSM
	// 3. ReviewService sends EventApproved back to FSM
	// 4. FSM transitions to Approved and emits OutboxNotify
	// 5. NotificationService receives and processes notification
	fmt.Println("\n=== Processing Review ===")
	reviewStart <- struct{}{}
	time.Sleep(300 * time.Millisecond)

	// Query current state (using StateQuery flag).
	fmt.Println("\n=== Checking Final State ===")
	resp2 := fsmRef.Ask(
		ctx, protofsm.ActorMessage[DocEvent]{
			StateQuery: true,
		},
	).Await(ctx)

	if resp2.IsOk() {
		state2, _ := resp2.Unpack()
		fmt.Printf("Final state: %s\n", state2.CurrentState.String())
		fmt.Printf("Is terminal: %v\n",
			state2.CurrentState.IsTerminal())
	}

	fmt.Println("\n=== Workflow Complete ===")

	// Output:
	// === Submitting Document ===
	// Document DOC-123 submitted by Alice
	//
	// === Processing Review ===
	// ReviewService: Processing document DOC-123 by Alice
	// Review started for document DOC-123
	// Document DOC-123 approved by ReviewBot
	// NotificationService: Document DOC-123 approved by ReviewBot
	//
	// === Checking Final State ===
	// Final state: Approved
	// Is terminal: true
	//
	// === Workflow Complete ===
}
