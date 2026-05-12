package darepod

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/round"
)

// TriggerRoundRegistration injects a IntentRequested event into the
// client round actor. The integration harness uses this to advance a queued
// round intent without reaching through private actor internals directly.
func (s *Server) TriggerRoundRegistration(ctx context.Context) error {
	if s.actorSystem == nil {
		return fmt.Errorf("actor system not initialized")
	}

	roundRef := round.NewServiceKey().Ref(s.actorSystem)
	future := roundRef.Ask(ctx, &round.ServerMessageNotification{
		Message: &round.IntentRequested{},
	})
	result := future.Await(ctx)
	if err := result.Err(); err != nil {
		return fmt.Errorf("failed to trigger round registration: %w",
			err)
	}

	return nil
}
