package waved

import (
	"context"
	"fmt"
	"testing"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/round"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestServerTriggerRoundRegistrationRequiresActorSystem verifies the harness
// helper fails fast before daemon startup has initialized the actor system.
func TestServerTriggerRoundRegistrationRequiresActorSystem(t *testing.T) {
	t.Parallel()

	server := &Server{}

	err := server.TriggerRoundRegistration(t.Context())
	require.EqualError(t, err, "actor system not initialized")
}

// TestServerTriggerRoundRegistration verifies the helper injects the expected
// local registration command through the round actor service key. A domain
// event wrapped as a server push cannot cross the durable mailbox codec.
func TestServerTriggerRoundRegistration(t *testing.T) {
	t.Parallel()

	system := actor.NewActorSystem()
	defer func() {
		err := system.Shutdown(t.Context())
		require.NoError(t, err)
	}()

	delivered := make(chan *round.TriggerRegistrationRequest, 1)
	key := round.NewServiceKey()
	behavior := actor.NewFunctionBehavior(
		func(_ context.Context,
			msg actormsg.RoundReceivable,
		) fn.Result[actormsg.RoundActorResp] {

			request, ok := msg.(*round.TriggerRegistrationRequest)
			if !ok {
				return fn.Err[actormsg.RoundActorResp](
					fmt.Errorf("expected local "+
						"registration, got %T", msg),
				)
			}

			delivered <- request

			return fn.Ok[actormsg.RoundActorResp](
				&round.ServerMessageResponse{},
			)
		},
	)
	_ = actor.RegisterWithSystem(system, "round-under-test", key, behavior)

	server := &Server{actorSystem: system}
	require.NoError(t, server.TriggerRoundRegistration(t.Context()))

	select {
	case <-delivered:
	case <-t.Context().Done():
		t.Fatal("round registration was not delivered")
	}
}
