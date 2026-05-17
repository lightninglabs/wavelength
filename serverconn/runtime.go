package serverconn

import (
	"context"
	"fmt"
	"sync"

	"github.com/lightninglabs/darepo-client/baselib/actor"
)

// RuntimeID returns the stable SQL outbox target ID used for serverconn egress.
func RuntimeID(localMailboxID string) string {
	return "serverconn-" + localMailboxID
}

// Runtime owns an in-memory serverconn actor and wires it together with the
// ingress loop and unary facade. Durable egress is provided by SQL outbox rows
// written by TellRef callers and drained by the shared actor outbox publisher.
type Runtime struct {
	connector *ServerConnectionActor
	unary     *UnaryFacade

	runtime *actor.Actor[ServerConnMsg, ServerConnResp]
	ref     actor.ActorRef[ServerConnMsg, ServerConnResp]
	tellRef actor.TellOnlyRef[ServerConnMsg]
	sqlRef  actor.ActorRef[ServerConnMsg, ServerConnResp]
	wg      *sync.WaitGroup

	egressWorker *MailboxEgressWorker
}

// NewRuntime constructs a serverconn runtime from connector config.
// The returned runtime is inert until Start is called.
func NewRuntime(cfg ConnectorConfig) (*Runtime, error) {
	if cfg.Transport == nil {
		return nil, fmt.Errorf("connector transport store is required")
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

	connector := NewServerConnectionActor(cfg)
	runtimeID := RuntimeID(cfg.LocalMailboxID)
	wg := &sync.WaitGroup{}
	runtime := actor.NewActor(actor.ActorConfig[
		ServerConnMsg, ServerConnResp,
	]{
		ID:          runtimeID,
		Behavior:    connector,
		MailboxSize: 128,
		Wg:          wg,
	})
	unary := NewUnaryFacade(connector)

	r := &Runtime{
		connector: connector,
		unary:     unary,
		runtime:   runtime,
		ref:       runtime.Ref(),
		wg:        wg,
	}
	transportRef := NewTransportTellRef(cfg)
	r.egressWorker = NewMailboxEgressWorker(MailboxEgressWorkerConfig{
		Store: cfg.Transport,
		Edge:  cfg.Edge,
		Log:   cfg.Log,
		Owner: runtimeID,
	})
	r.tellRef = transportRef
	r.sqlRef = transportRef

	return r, nil
}

// Start launches egress processing and ingress pulling. Returns an
// error if the ingress checkpoint cannot be loaded from the store.
func (r *Runtime) Start(ctx context.Context) error {
	r.StartEgress(ctx)

	if err := r.StartIngress(ctx); err != nil {
		r.runtime.Stop()

		return err
	}

	return nil
}

// StartEgress launches in-memory egress processing without starting ingress.
// Callers that need local actors registered before remote mailbox replay can
// use this to bring up outbound delivery first and start ingress later.
func (r *Runtime) StartEgress(ctx context.Context) {
	r.runtime.Start()
	if err := r.egressWorker.Start(ctx); err != nil {
		r.connector.log.WarnS(ctx, "Unable to start egress worker",
			err,
		)
	}
}

// StartIngress launches ingress pulling and heartbeat handling.
func (r *Runtime) StartIngress(ctx context.Context) error {
	return r.connector.StartIngress(ctx)
}

// Stop shuts down ingress polling and egress processing.
func (r *Runtime) Stop() {
	r.connector.StopIngress()
	r.egressWorker.Stop()
	r.runtime.Stop()
}

// StopAndWait shuts down ingress polling and waits for egress processing to
// exit.
func (r *Runtime) StopAndWait(ctx context.Context) error {
	r.connector.StopIngress()
	r.egressWorker.Stop()
	r.runtime.Stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.wg.Wait()
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()

	case <-done:
		return nil
	}
}

// Unary returns the unary RPC facade bound to this runtime.
func (r *Runtime) Unary() *UnaryFacade {
	return r.unary
}

// Connector returns the underlying connector behavior.
func (r *Runtime) Connector() *ServerConnectionActor {
	return r.connector
}

// Ref returns the direct in-memory actor ref for request/response calls and
// outbox publisher delivery.
func (r *Runtime) Ref() actor.ActorRef[ServerConnMsg, ServerConnResp] {
	return r.ref
}

// TransportRef returns the SQL transport ref used by legacy actor outbox
// publishers while callers are being moved to mailbox_egress directly.
func (r *Runtime) TransportRef() actor.ActorRef[ServerConnMsg, ServerConnResp] {
	return r.sqlRef
}

// TellRef returns a mailbox_egress-backed fire-and-forget ref. A nil return
// from Tell means the egress intent was durably recorded.
func (r *Runtime) TellRef() actor.TellOnlyRef[ServerConnMsg] {
	return r.tellRef
}
