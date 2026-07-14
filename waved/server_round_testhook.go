package waved

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/round"
)

// TriggerRoundRegistration injects an IntentRequested event into the client
// round actor. The RPC server and integration harness use this to advance a
// queued round intent without reaching through private actor internals
// directly.
//
// IntentRequested is fire-and-forget from the caller's perspective: once the
// FSM accepts the event it runs the registration handshake against the
// operator in the round actor's own turn loop. The caller's context is
// plumbed into the actor envelope as callerCtx and is what the FSM's
// downstream forfeit-VTXO lookup (during JoinRoundRequest validation)
// observes. Reusing the caller's ctx there means an RPC return cancels the
// ctx mid-handshake and the round transitions to ClientFailed with "context
// canceled". Detach the Ask with context.WithoutCancel so the trigger
// reaches the actor regardless of the caller's lifetime, while the round's
// own lifetime continues to be governed by the actor system's shutdown.
//
// The Await keeps the original ctx because Await's ctx is purely local — it
// only controls how long this goroutine blocks at the promise's channel and
// never reaches the actor. Using the caller's ctx here lets the RPC handler
// unblock promptly if the caller disconnects, while the FSM continues
// processing in the background under askCtx.
func (s *Server) TriggerRoundRegistration(ctx context.Context) error {
	if s.actorSystem == nil {
		return fmt.Errorf("actor system not initialized")
	}

	askCtx := context.WithoutCancel(ctx)

	roundRef := round.NewServiceKey().Ref(s.actorSystem)
	future := roundRef.Ask(askCtx, &round.ServerMessageNotification{
		Message: &round.IntentRequested{},
	})
	result := future.Await(ctx)
	if err := result.Err(); err != nil {
		return fmt.Errorf("failed to trigger round registration: %w",
			err)
	}

	return nil
}
