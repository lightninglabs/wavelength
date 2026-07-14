package serverconn

import (
	"context"
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
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

	// Mailbox transport v1 is the stable bootstrap endpoint represented by
	// a code constant. Default it when unset so callers cannot accidentally
	// start a runtime with a zero transport version.
	if cfg.MailboxProtocolVersion == 0 {
		cfg.MailboxProtocolVersion = mailboxpb.MailboxProtocolVersionV1
	}

	// The Ark protocol version is negotiated before the runtime exists and
	// is bound here for the runtime's lifetime. A zero value means no
	// version was selected, so refuse to construct the runtime.
	if cfg.ArkProtocolVersion == 0 {
		return nil, fmt.Errorf("ark protocol version must be non-zero")
	}

	if cfg.Codec == nil {
		cfg.Codec = NewServerConnCodec()
	}

	connector := NewServerConnectionActor(cfg)

	durableCfg := actor.DefaultDurableTxActorConfig[
		ServerConnMsg, ServerConnResp, egressTx,
	](
		DurableActorID(cfg.LocalMailboxID), connector,
		connector.bindStores, cfg.Store, cfg.Codec,
	)
	durableCfg.Log = cfg.Log

	// Run the egress sender as a competing-consumer pool. NewDurableActor
	// clamps a zero or negative count up to a single worker, preserving the
	// historical single-sender behavior for callers that leave it unset.
	durableCfg.NumWorkers = cfg.EgressWorkers

	// A permanent version error is not retryable: dead-letter the failing
	// durable message immediately instead of retrying it forever. All other
	// (transient) failures keep the default exponential-backoff policy.
	durableCfg.TellRetryPolicy = permanentAwareTellRetryPolicy

	durable, err := actor.NewDurableActor(durableCfg).Unpack()
	if err != nil {
		return nil, err
	}
	unary := NewUnaryFacade(connector)

	return &Runtime{
		DurableActor: durable,
		connector:    connector,
		unary:        unary,
	}, nil
}

// permanentAwareTellRetryPolicy is the durable actor's retry policy for
// serverconn egress. A permanent version error is non-retryable, so the
// failing durable message is dead-lettered immediately rather than retried
// forever; every other (transient) failure keeps the default
// exponential-backoff schedule.
func permanentAwareTellRetryPolicy(err error,
	attempts int) (bool, time.Duration) {

	if mailboxconn.IsPermanentVersionError(err) {
		return false, 0
	}

	return actor.DefaultTellRetryPolicy(err, attempts)
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

// MarkIncompatible drives the runtime to its terminal incompatible state. It
// is the exported entry point for callers outside the connector (such as a
// refresh-only GetInfo that observes a changed selection) that detect a
// permanent version failure on a side channel rather than on the mailbox edge.
func (r *Runtime) MarkIncompatible(ctx context.Context,
	statusErr *mailboxconn.StatusError) {

	r.connector.markIncompatible(ctx, statusErr)
}

// StampEnvelope stamps the runtime's immutable mailbox transport and Ark
// protocol versions onto a locally constructed envelope immediately before it
// is sent. It is the shared stamping entry point for callers outside the
// connector (such as the waved mailbox response path) so every client send
// path carries the runtime-bound version pair rather than copying a
// caller-controlled value.
func (r *Runtime) StampEnvelope(env *mailboxpb.Envelope) {
	r.connector.cfg.stampEnvelope(env)
}
