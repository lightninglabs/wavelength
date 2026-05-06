package serverconn

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/baselib/actor"
)

// DurableActorID returns the durable actor mailbox ID used for serverconn
// ingress checkpointing and egress mailbox persistence.
func DurableActorID(localMailboxID string) string {
	return "serverconn-" + localMailboxID
}

// Runtime embeds a DurableActor for serverconn egress and wires it together
// with the ingress loop and unary facade. Because the DurableActor is
// embedded, Runtime can be registered directly with the actor system — Ref
// and TellRef are promoted without wrapper methods.
type Runtime struct {
	*actor.DurableActor[ServerConnMsg, ServerConnResp]

	connector *ServerConnectionActor
	unary     *UnaryFacade
}

// NewRuntime constructs a durable serverconn runtime from connector config.
// The returned runtime is inert until Start is called.
func NewRuntime(cfg ConnectorConfig) (*Runtime, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("connector store is required")
	}

	if cfg.Edge == nil {
		return nil, fmt.Errorf("connector edge is required")
	}

	if cfg.LocalMailboxID == "" {
		return nil, fmt.Errorf("local mailbox id is required")
	}

	if cfg.RemoteMailboxID == "" {
		return nil, fmt.Errorf("remote mailbox id is required")
	}

	if cfg.Codec == nil {
		cfg.Codec = NewServerConnCodec()
	}

	connector := NewServerConnectionActor(cfg)

	durableCfg := actor.DefaultDurableActorConfig[
		ServerConnMsg, ServerConnResp,
	](
		DurableActorID(cfg.LocalMailboxID),
		connector,
		cfg.Store,
		cfg.Codec,
	)
	durableCfg.Log = cfg.Log

	durable := actor.NewDurableActor(durableCfg)
	unary := NewUnaryFacade(connector)

	return &Runtime{
		DurableActor: durable,
		connector:    connector,
		unary:        unary,
	}, nil
}

// Start launches durable egress processing and ingress pulling. Returns an
// error if the ingress checkpoint cannot be loaded from the store.
func (r *Runtime) Start(ctx context.Context) error {
	//nolint:contextcheck // durable actor owns lifecycle
	r.StartEgress()

	if err := r.StartIngress(ctx); err != nil {
		r.DurableActor.Stop()

		return err
	}

	return nil
}

// StartEgress launches durable egress processing without starting ingress.
// Callers that need local actors registered before remote mailbox replay can
// use this to bring up outbound delivery first and start ingress later.
func (r *Runtime) StartEgress() {
	r.DurableActor.Start()
}

// StartIngress launches ingress pulling and heartbeat handling.
func (r *Runtime) StartIngress(ctx context.Context) error {
	return r.connector.StartIngress(ctx)
}

// Stop shuts down ingress polling and durable egress processing.
func (r *Runtime) Stop() {
	r.connector.StopIngress()
	r.DurableActor.Stop()
}

// StopAndWait shuts down ingress polling and waits for durable egress
// processing to exit.
func (r *Runtime) StopAndWait(ctx context.Context) error {
	r.connector.StopIngress()

	return r.DurableActor.StopAndWait(ctx)
}

// Unary returns the unary RPC facade bound to this runtime.
func (r *Runtime) Unary() *UnaryFacade {
	return r.unary
}

// Connector returns the underlying connector behavior.
func (r *Runtime) Connector() *ServerConnectionActor {
	return r.connector
}
