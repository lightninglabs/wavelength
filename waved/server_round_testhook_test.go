package waved

import (
	"context"
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
// IntentRequested event through the round actor service key.
func TestServerTriggerRoundRegistration(t *testing.T) {
	t.Parallel()

	system := actor.NewActorSystem()
	defer func() {
		err := system.Shutdown(t.Context())
		require.NoError(t, err)
	}()

	delivered := make(chan *round.ServerMessageNotification, 1)
	key := round.NewServiceKey()
	behavior := actor.NewFunctionBehavior(
		func(_ context.Context,
			msg actormsg.RoundReceivable,
		) fn.Result[actormsg.RoundActorResp] {

			notif, ok := msg.(*round.ServerMessageNotification)
			require.True(
				t, ok, "expected server notification, got %T",
				msg,
			)

			delivered <- notif

			return fn.Ok[actormsg.RoundActorResp](
				&round.ServerMessageResponse{},
			)
		},
	)
	_ = actor.RegisterWithSystem(system, "round-under-test", key, behavior)

	server := &Server{actorSystem: system}
	require.NoError(t, server.TriggerRoundRegistration(t.Context()))

	select {
	case notif := <-delivered:
		require.IsType(t, &round.IntentRequested{}, notif.Message)

	case <-t.Context().Done():
		t.Fatal("round registration was not delivered")
	}
}
