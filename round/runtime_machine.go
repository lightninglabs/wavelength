package round

import (
	"context"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
)

// roundStateMachine lets durable owners run transitions inline without changing
// the ordinary daemon's independently driven state machines.
type roundStateMachine interface {
	// Start admits events for the owner's lifetime.
	Start(context.Context)

	// Stop permanently closes event admission.
	Stop()

	// AskEvent completes an event and its internal transition chain.
	AskEvent(context.Context, ClientEvent) actor.Future[[]ClientOutMsg]

	// CurrentState returns the currently published state with a bounded
	// wait.
	CurrentState() (
		protofsm.State[ClientEvent, ClientOutMsg, *ClientEnvironment],
		error)

	// CurrentStateWithContext reads state within the supplied budget.
	CurrentStateWithContext(context.Context) (
		protofsm.State[ClientEvent, ClientOutMsg, *ClientEnvironment],
		error)
}

// newStateMachine selects the runner at the owning actor boundary. Only the
// durable runtime opts into synchronous transitions and checkpoint ownership.
func (a *RoundClientActor) newStateMachine(ctx context.Context,
	cfg ClientStateMachineCfg) roundStateMachine {

	var machine roundStateMachine
	if a.durable {
		machine = protofsm.NewInlineStateMachine(cfg)
	} else {
		threaded := protofsm.NewStateMachine(cfg)
		machine = &threaded
	}
	a.startRoundFSM(ctx, machine)

	return machine
}
