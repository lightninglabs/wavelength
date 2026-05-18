package oor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/build"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// ClientCoordinator runs the OOR client workflow without a local actor
// mailbox.
//
// It keeps the existing deterministic FSM handlers for now, but the caller
// invokes it directly.
type ClientCoordinator struct {
	cfg      ClientActorCfg
	behavior *oorDurableBehavior
}

// CoordinatorTellRef adapts a direct coordinator to actor.TellOnlyRef so
// existing callback producers can target it while their actor dependencies are
// unwound.
type CoordinatorTellRef struct {
	id string

	mu    sync.RWMutex
	coord *ClientCoordinator
}

// NewCoordinatorTellRef creates an initially unbound coordinator ref.
func NewCoordinatorTellRef(id string) *CoordinatorTellRef {
	if id == "" {
		id = OORActorServiceKeyName
	}

	return &CoordinatorTellRef{id: id}
}

// Bind points the ref at a live coordinator.
func (r *CoordinatorTellRef) Bind(coord *ClientCoordinator) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.coord = coord
}

// ID returns the stable reference identifier.
func (r *CoordinatorTellRef) ID() string {
	return r.id
}

// Tell drives one coordinator message synchronously.
func (r *CoordinatorTellRef) Tell(ctx context.Context, msg ActorMsg) error {
	r.mu.RLock()
	coord := r.coord
	r.mu.RUnlock()

	if coord == nil {
		return fmt.Errorf("OOR coordinator ref %q is not bound", r.id)
	}

	result := coord.Handle(ctx, msg)
	if result.IsErr() {
		return result.Err()
	}

	return nil
}

// NewClientCoordinator creates an actorless OOR client coordinator.
func NewClientCoordinator(cfg ClientActorCfg) *ClientCoordinator {
	cfg.Limits = normalizeReceiveLimits(cfg.Limits)
	if cfg.ActorID == "" {
		cfg.ActorID = fmt.Sprintf("oor-client-%s", uuid.NewString())
	}

	return &ClientCoordinator{
		cfg: cfg,
		behavior: &oorDurableBehavior{
			cfg:      cfg,
			sessions: make(map[SessionID]*sessionHandle),
			pendingIncoming: make(
				map[SessionID]*ResolveIncomingTransferRequest,
			),
		},
	}
}

// Start restores SQL-backed sessions and resumes any pending outbox work.
func (c *ClientCoordinator) Start(ctx context.Context) error {
	if c == nil || c.behavior == nil {
		return fmt.Errorf("coordinator must be provided")
	}

	if c.cfg.SessionStore == nil {
		return fmt.Errorf("session store must be provided")
	}

	result := c.behavior.restoreSessionState(ctx)
	if result.IsErr() {
		return result.Err()
	}

	c.cfg.Log.UnwrapOr(build.LoggerFromContext(ctx)).InfoS(
		ctx,
		"OOR client coordinator started",
		slog.String("actor_id", c.cfg.ActorID),
	)

	return nil
}

// Stop is present for lifecycle symmetry with OORClientActor.
func (c *ClientCoordinator) Stop() {}

// Receive processes one workflow request synchronously.
func (c *ClientCoordinator) Receive(ctx context.Context,
	msg ActorMsg) fn.Result[ActorResp] {

	return c.Handle(ctx, msg)
}

// Handle processes one workflow message synchronously.
func (c *ClientCoordinator) Handle(ctx context.Context,
	msg ActorMsg) fn.Result[ActorResp] {

	if c == nil || c.behavior == nil {
		return fn.Err[ActorResp](
			fmt.Errorf("coordinator must be provided"),
		)
	}
	if msg == nil {
		return fn.Err[ActorResp](fmt.Errorf("message must be provided"))
	}

	ctx = build.ContextWithLogger(
		ctx,
		c.cfg.Log.UnwrapOr(
			build.LoggerFromContext(ctx),
		),
	)

	return c.behavior.Receive(ctx, msg)
}

// ProcessOORClientEffect executes one claimed SQL effect against the current
// in-memory session handle.
func (c *ClientCoordinator) ProcessOORClientEffect(ctx context.Context,
	effect OORClientEffect) error {

	if c == nil || c.behavior == nil {
		return fmt.Errorf("coordinator must be provided")
	}

	ctx = build.ContextWithLogger(
		ctx,
		c.cfg.Log.UnwrapOr(
			build.LoggerFromContext(ctx),
		),
	)

	return c.behavior.processSQLClientEffect(ctx, effect)
}

var _ OORClientEffectProcessor = (*ClientCoordinator)(nil)
