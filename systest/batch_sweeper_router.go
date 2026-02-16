//go:build systest

package systest

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo/batchwatcher"
)

// BatchSweeperRouter fan-outs BatchWatcher notifications to one or more
// BatchSweeperMsg consumers. This is used in systests to keep the existing
// mock sweeper assertions while also wiring a real BatchSweeperActor.
type BatchSweeperRouter struct {
	mu sync.Mutex

	targets []actor.TellOnlyRef[batchwatcher.BatchSweeperMsg]
}

// NewBatchSweeperRouter creates a new router that forwards to the provided
// initial targets.
func NewBatchSweeperRouter(
	targets ...actor.TellOnlyRef[batchwatcher.BatchSweeperMsg],
) *BatchSweeperRouter {

	return &BatchSweeperRouter{
		targets: targets,
	}
}

// ID implements actor.BaseActorRef.
func (r *BatchSweeperRouter) ID() string {
	return "batch-sweeper-router"
}

// Tell forwards the notification to all configured targets.
func (r *BatchSweeperRouter) Tell(ctx context.Context,
	msg batchwatcher.BatchSweeperMsg) error {

	r.mu.Lock()
	targets := append(
		[]actor.TellOnlyRef[batchwatcher.BatchSweeperMsg](nil),
		r.targets...,
	)
	r.mu.Unlock()

	var errs []error

	for _, target := range targets {
		if target == nil {
			continue
		}

		err := target.Tell(ctx, msg)
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"batch sweeper target %s: %w", target.ID(), err,
			))
		}
	}

	return errors.Join(errs...)
}

// SetTargets replaces the set of notification recipients.
func (r *BatchSweeperRouter) SetTargets(
	targets ...actor.TellOnlyRef[batchwatcher.BatchSweeperMsg]) {

	r.mu.Lock()
	r.targets = targets
	r.mu.Unlock()
}
