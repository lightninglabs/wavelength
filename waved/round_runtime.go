package waved

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/round"
)

// startDurableRoundActor registers native inbox and outbox references before
// startup can publish restored effects or request the wallet's boarding
// backlog.
func (s *Server) startDurableRoundActor(ctx context.Context,
	cfg *round.RoundClientConfig) error {

	//nolint:contextcheck // The runtime owns an independent actor lifetime.
	runtime, err := round.NewDurableRoundClientActor(cfg, s.deliveryStore)
	if err != nil {
		return err
	}
	s.durableRoundActor = runtime
	if err := actor.RegisterWithReceptionist(
		s.actorSystem.Receptionist(), round.NewServiceKey(),
		runtime.Ref(),
	); err != nil {
		return err
	}
	if err := actor.RegisterWithReceptionist(
		s.actorSystem.Receptionist(),
		actor.NewServiceKey[actor.Message, any](cfg.Name),
		runtime.OutboxRef(),
	); err != nil {
		return err
	}
	if err := runtime.Start(ctx); err != nil {
		return fmt.Errorf("start durable round runtime: %w", err)
	}

	return nil
}
