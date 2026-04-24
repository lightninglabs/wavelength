package clientconn

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/baselib/actor"
)

// DurableActorID returns the durable actor mailbox ID used for per-client
// ingress checkpointing and egress mailbox persistence. Each client gets
// a unique actor ID derived from the server's per-client mailbox
// identifier.
func DurableActorID(localMailboxID string) string {
	return "clientconn-" + localMailboxID
}

// ClientRuntime embeds a DurableActor for per-client egress and wires it
// together with the ingress loop and unary facade. Because the
// DurableActor is embedded, ClientRuntime can be registered directly with
// the actor system — Ref and TellRef are promoted without wrapper
// methods.
//
// Each registered client gets its own ClientRuntime instance, managed by
// the ClientsConnBridge.
type ClientRuntime struct {
	*actor.DurableActor[connectorMsg, connectorResp]

	connector *ClientConnectionActor
	unary     *UnaryFacade
}

// NewClientRuntime constructs a durable per-client runtime from the
// given configuration. The returned runtime is inert until Start is
// called.
func NewClientRuntime(cfg PerClientConfig) (*ClientRuntime, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("connector store is required")
	}

	if cfg.Edge == nil {
		return nil, fmt.Errorf("connector edge is required")
	}

	if cfg.LocalMailboxID == "" {
		return nil, fmt.Errorf(
			"local mailbox id is required",
		)
	}

	if cfg.RemoteMailboxID == "" {
		return nil, fmt.Errorf(
			"remote mailbox id is required",
		)
	}

	// Reject identical local/remote mailbox IDs to prevent a
	// self-loop where the ingress loop pulls messages the egress
	// just sent.
	if cfg.LocalMailboxID == cfg.RemoteMailboxID {
		return nil, fmt.Errorf(
			"local and remote mailbox ids must differ, "+
				"both are %q", cfg.LocalMailboxID,
		)
	}

	if len(cfg.Dispatchers) == 0 {
		return nil, fmt.Errorf(
			"dispatchers map is required (non-empty)",
		)
	}

	if cfg.ProtocolVersion == 0 {
		return nil, fmt.Errorf(
			"protocol version must be set (non-zero)",
		)
	}

	if cfg.Codec == nil {
		cfg.Codec = newClientConnCodec()
	}

	connector := NewClientConnectionActor(cfg)

	durableCfg := actor.DefaultDurableActorConfig[
		connectorMsg, connectorResp,
	](
		DurableActorID(cfg.LocalMailboxID),
		connector,
		cfg.Store,
		cfg.Codec,
	)

	durable := actor.NewDurableActor(durableCfg)
	unary := NewUnaryFacade(connector)

	return &ClientRuntime{
		DurableActor: durable,
		connector:    connector,
		unary:        unary,
	}, nil
}

// Start launches durable egress processing and ingress pulling for this
// client. Returns an error if the ingress checkpoint cannot be loaded
// from the store.
func (r *ClientRuntime) Start(ctx context.Context) error {
	r.DurableActor.Start()

	if err := r.connector.StartIngress(ctx); err != nil {
		r.DurableActor.Stop()

		return err
	}

	return nil
}

// Stop shuts down ingress polling and durable egress processing for this
// client.
func (r *ClientRuntime) Stop() {
	r.connector.StopIngress()
	r.DurableActor.Stop()
}

// StopAndWait shuts down ingress polling and durable egress processing, then
// waits for the durable actor loop to exit.
func (r *ClientRuntime) StopAndWait(ctx context.Context) error {
	r.connector.StopIngress()

	if err := r.DurableActor.StopAndWait(ctx); err != nil {
		return fmt.Errorf("stop durable actor: %w", err)
	}

	return nil
}

// Unary returns the unary RPC facade bound to this client's runtime.
func (r *ClientRuntime) Unary() *UnaryFacade {
	return r.unary
}

// Connector returns the underlying per-client connector behavior.
func (r *ClientRuntime) Connector() *ClientConnectionActor {
	return r.connector
}
