package actor_test

import (
	"context"
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// BasicGreetingMsg is a simple message type for the basic actor example.
type BasicGreetingMsg struct {
	actor.BaseMessage
	Name string
}

// MessageType implements actor.Message.
func (m BasicGreetingMsg) MessageType() string { return "BasicGreetingMsg" }

// BasicGreetingResponse is a simple response type.
type BasicGreetingResponse struct {
	Greeting string
}

// ExampleActor demonstrates creating a single actor, sending it a message
// directly using Ask, and then unregistering it from service discovery.
func ExampleActor() {
	system := actor.NewActorSystem()
	defer system.Shutdown(context.Background())

	//nolint:ll
	greeterKey := actor.NewServiceKey[
		BasicGreetingMsg,
		BasicGreetingResponse,
	](
		"basic-greeter",
	)

	actorID := "my-greeter"
	greeterBehavior := actor.NewFunctionBehavior(
		func(ctx context.Context,
			msg BasicGreetingMsg) fn.Result[BasicGreetingResponse] {

			return fn.Ok(BasicGreetingResponse{
				Greeting: "Hello, " + msg.Name + " from " +
					actorID,
			})
		},
	)

	// Spawn the actor. This registers it with the system and receptionist,
	// and starts it. It returns an ActorRef.
	greeterRef := greeterKey.Spawn(system, actorID, greeterBehavior)
	fmt.Printf("Actor %s spawned.\n", greeterRef.ID())

	// Send a message directly to the actor's reference.
	askCtx, askCancel := context.WithTimeout(
		context.Background(), 1*time.Second,
	)
	defer askCancel()
	futureResponse := greeterRef.Ask(
		askCtx, BasicGreetingMsg{
			Name: "World",
		},
	)

	awaitCtx, awaitCancel := context.WithTimeout(
		context.Background(), 1*time.Second,
	)
	defer awaitCancel()
	result := futureResponse.Await(awaitCtx)

	result.WhenErr(func(err error) {
		fmt.Printf("Error awaiting response: %v\n", err)
	})
	result.WhenOk(func(response BasicGreetingResponse) {
		fmt.Printf("Received: %s\n", response.Greeting)
	})

	// Unregister the actor from the receptionist. This removes it from
	// service discovery but does NOT stop the actor. To stop the actor,
	// use StopAndRemoveActor or let Shutdown handle it.
	unregistered := greeterKey.Unregister(system, greeterRef)
	if unregistered {
		fmt.Printf("Actor %s unregistered from receptionist.\n",
			greeterRef.ID())
	} else {
		fmt.Printf("Failed to unregister actor %s.\n", greeterRef.ID())
	}

	// Verify it's no longer in the receptionist.
	refsAfterUnregister := actor.FindInReceptionist(
		system.Receptionist(), greeterKey,
	)
	fmt.Printf("Actors for key '%s' after unregister: %d\n",
		"basic-greeter", len(refsAfterUnregister))

	// The deferred system.Shutdown() will stop all actors when this
	// function returns.

	// Output:
	// Actor my-greeter spawned.
	// Received: Hello, World from my-greeter
	// Actor my-greeter unregistered from receptionist.
	// Actors for key 'basic-greeter' after unregister: 0
}
