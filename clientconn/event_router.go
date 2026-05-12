package clientconn

import (
	"context"
	"fmt"
	"sync"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"google.golang.org/protobuf/proto"
)

// InboundClientMessage is implemented by actor messages that arrive from
// a client via the per-client mailbox ingress loop. FromProto populates
// the receiver from the deserialized proto event, completing the
// bidirectional proto↔actor message conversion pair.
type InboundClientMessage interface {
	// FromProto populates the receiver from a client-pushed proto
	// message. It is called by the EventRouter dispatch closure after
	// the envelope body has been unmarshaled into the expected proto
	// type. Return an error to reject events whose proto fields cannot
	// be converted.
	FromProto(proto.Message) error
}

// InboundActorMessage is the type-constraint for actor messages that
// arrive from a client. It combines actor.Message (for dispatch via the
// actor system's Receptionist) with InboundClientMessage (for proto
// deserialization). Types that implement this constraint can be used with
// the NewEventRoute helper.
type InboundActorMessage interface {
	actor.Message
	InboundClientMessage
}

// EventRouteConfig holds parameters for registering a single typed event
// route with an EventRouter.
//
// M is the actor message type that the target actor accepts. R is the
// response type (typically unused for fire-and-forget events but required
// by the actor framework's ServiceKey generic constraint).
type EventRouteConfig[M actor.Message, R any] struct {
	// Service is the fully-qualified protobuf service name that
	// appears in the inbound envelope's RPC metadata
	// (e.g., "roundtest.v1.RoundNotifyService").
	Service string

	// Method is the protobuf method name (e.g., "NotifyRoundStarted").
	Method string

	// NewEvent must return a fresh zero-value proto.Message for the
	// expected event type. It is called once per delivered envelope to
	// provide the unmarshal target.
	NewEvent func() proto.Message

	// Key is the ServiceKey for the target durable actor. The router
	// calls key.Ref(system).Tell(ctx, msg) for each dispatched event,
	// which persists the message to the actor's durable mailbox
	// before returning nil.
	Key actor.ServiceKey[M, R]

	// Adapt converts the deserialized proto.Message to the actor
	// message type M. Return an error to reject envelopes whose body
	// cannot be converted.
	Adapt func(proto.Message) (M, error)
}

// EventRouter maps inbound KIND_REQUEST and KIND_EVENT envelope routes to
// typed durable actor mailboxes via ServiceKey.
//
// EventRouter resolves target actors through the actor system's
// Receptionist, guaranteeing durable delivery before returning from each
// dispatch call.
//
// At wiring time, callers call AddRoute for each (service, method) pair
// they want to handle, then pass AsDispatcherMap() to
// PerClientConfig.Dispatchers.
type EventRouter struct {
	mu     sync.RWMutex
	system actor.SystemContext
	routes map[mailboxrpc.ServiceMethod]EnvelopeDispatcher
}

// NewEventRouter creates an empty EventRouter backed by the given actor
// system. The system is used to resolve ServiceKeys to actor references
// at dispatch time.
func NewEventRouter(system actor.SystemContext) *EventRouter {
	return &EventRouter{
		system: system,
		routes: make(
			map[mailboxrpc.ServiceMethod]EnvelopeDispatcher,
		),
	}
}

// AddRoute registers a typed event route with the router. The generic
// parameters [M, R] must match the ServiceKey's type parameters.
//
// AddRoute is a thin wrapper around AddEnvelopeRoute that discards
// the envelope in the Adapt closure. Use AddRoute when the handler
// does not need transport metadata from the envelope.
//
// Registration is idempotent — re-registering the same (service,
// method) pair replaces the previous route.
func AddRoute[M actor.Message, R any](r *EventRouter,
	cfg EventRouteConfig[M, R]) {

	adapt := cfg.Adapt

	AddEnvelopeRoute(r, EnvelopeRouteConfig[M, R]{
		Service:  cfg.Service,
		Method:   cfg.Method,
		NewEvent: cfg.NewEvent,
		Key:      cfg.Key,
		Adapt: func(_ *mailboxpb.Envelope, p proto.Message) (M, error) {
			return adapt(p)
		},
	})
}

// InboundEventRouteConfig holds parameters for registering an event route
// where the actor message type implements InboundActorMessage.
// NewEventRoute auto-generates the Adapt closure from M.FromProto, so
// callers don't need to write one manually.
type InboundEventRouteConfig[M InboundActorMessage, R any] struct {
	// Service is the fully-qualified protobuf service name
	// (e.g., "roundtest.v1.RoundNotifyService").
	Service string

	// Method is the protobuf method name (e.g., "NotifyRoundStarted").
	Method string

	// Key is the ServiceKey for the target durable actor.
	Key actor.ServiceKey[M, R]

	// NewEvent must return a fresh zero-value proto.Message for the
	// expected event type.
	NewEvent func() proto.Message

	// NewMsg must return a non-nil zero-value M. FromProto is called
	// on the returned value to populate it from the deserialized
	// event.
	NewMsg func() M
}

// NewEventRoute registers a typed event route for actor message types
// that implement InboundActorMessage. It auto-generates the Adapt closure
// from M.FromProto, eliminating boilerplate for the common case where the
// actor message knows how to deserialize itself from proto.
func NewEventRoute[M InboundActorMessage, R any](r *EventRouter,
	cfg InboundEventRouteConfig[M, R]) {

	if cfg.NewMsg == nil {
		panic("clientconn: nil NewMsg in " +
			"InboundEventRouteConfig")
	}

	newMsg := cfg.NewMsg

	AddRoute(r, EventRouteConfig[M, R]{
		Service:  cfg.Service,
		Method:   cfg.Method,
		NewEvent: cfg.NewEvent,
		Key:      cfg.Key,
		Adapt: func(p proto.Message) (M, error) {
			m := newMsg()

			return m, m.FromProto(p)
		},
	})
}

// EnvelopeRouteConfig holds parameters for registering a typed event
// route that has access to the full inbound envelope. This extends
// EventRouteConfig by passing the envelope to the Adapt closure, which
// allows extracting transport-level metadata (e.g., the envelope sender
// for ClientID injection) that is not part of the proto body.
//
// Use this for server-side inbound client request dispatch where the
// handler needs to know which client sent the message.
//
// NOTE: env.Sender is client-controlled at the mailbox transport
// layer. Callers must NOT trust env.Sender as an authenticated
// identity without server-side stamping. See
// RegisterClientWithAllDispatchers in server_indexer.go, which
// overwrites env.Sender with the server-authenticated clientID
// before dispatch.
type EnvelopeRouteConfig[M actor.Message, R any] struct {
	// Service is the fully-qualified protobuf service name.
	Service string

	// Method is the protobuf method name.
	Method string

	// NewEvent must return a fresh zero-value proto.Message for the
	// expected request type.
	NewEvent func() proto.Message

	// Key is the ServiceKey for the target actor.
	Key actor.ServiceKey[M, R]

	// Adapt converts the deserialized proto and envelope metadata
	// into the actor message type M. The envelope is provided so
	// the closure can extract the sender (client ID) or other
	// transport-level fields.
	Adapt func(*mailboxpb.Envelope, proto.Message) (M, error)
}

// AddEnvelopeRoute registers a typed event route that receives the full
// inbound envelope in its Adapt closure. This is the server-side
// counterpart of AddRoute: while AddRoute only passes the deserialized
// proto to Adapt, AddEnvelopeRoute also passes the envelope so the
// handler can extract transport metadata like the client's mailbox ID.
//
// Like AddRoute, this is a package-level generic function because Go
// does not allow methods with type parameters on non-generic types.
func AddEnvelopeRoute[M actor.Message, R any](r *EventRouter,
	cfg EnvelopeRouteConfig[M, R]) {

	if cfg.Service == "" {
		panic(
			"clientconn: empty service name in EnvelopeRouteConfig",
		)
	}
	if cfg.Method == "" {
		panic(
			"clientconn: empty method name in EnvelopeRouteConfig",
		)
	}
	if cfg.NewEvent == nil {
		panic("clientconn: nil NewEvent in " +
			"EnvelopeRouteConfig")
	}
	if cfg.Adapt == nil {
		panic("clientconn: nil Adapt in " +
			"EnvelopeRouteConfig")
	}

	system := r.system
	actorKey := cfg.Key

	dispatcher := func(ctx context.Context, env *mailboxpb.Envelope) error {
		if env == nil {
			return fmt.Errorf("nil envelope for %s/%s", cfg.Service,
				cfg.Method)
		}

		// Error responses are encoded in headers
		// and intentionally omit the response
		// body. Let the adapter inspect those
		// headers so it can translate
		// transport-level RPC failures into
		// actor events.
		var event proto.Message
		if env.Body != nil {
			// Deserialize the envelope body into the registered
			// proto type.
			event = cfg.NewEvent()
			if event == nil {
				return fmt.Errorf("nil event prototype for "+
					"%s/%s", cfg.Service, cfg.Method)
			}

			if err := (proto.UnmarshalOptions{
				DiscardUnknown: true,
			}).Unmarshal(env.Body.Value,
				event,
			); err != nil {
				return fmt.Errorf("unmarshal %s/%s event: %w",
					cfg.Service, cfg.Method, err)
			}
		} else if mailboxrpc.DecodeErrorHeaders(env.Headers) == nil {
			return fmt.Errorf("nil envelope body for %s/%s",
				cfg.Service, cfg.Method)
		}

		// Convert the proto event and envelope metadata to
		// the actor message type. The envelope is passed so
		// the closure can extract the sender for ClientID.
		actorMsg, err := cfg.Adapt(env, event)
		if err != nil {
			return fmt.Errorf("adapt %s/%s event: %w", cfg.Service,
				cfg.Method, err)
		}

		return actorKey.Ref(system).Tell(ctx, actorMsg)
	}

	serviceMethod := mailboxrpc.ServiceMethod{
		Service: cfg.Service,
		Method:  cfg.Method,
	}

	r.mu.Lock()
	r.routes[serviceMethod] = dispatcher
	r.mu.Unlock()
}

// DispatcherMap maps envelope routing keys to their dispatch closures. It
// is the type returned by AsDispatcherMap and consumed by
// PerClientConfig.Dispatchers.
type DispatcherMap = map[mailboxrpc.ServiceMethod]EnvelopeDispatcher

// AsDispatcherMap returns a shallow copy of the registered routes as a
// DispatcherMap suitable for use as PerClientConfig.Dispatchers.
//
// The returned map is safe to read concurrently. Callers should call
// AsDispatcherMap after all routes have been registered, before
// constructing the PerClientConfig.
func (r *EventRouter) AsDispatcherMap() DispatcherMap {
	r.mu.RLock()
	defer r.mu.RUnlock()

	m := make(
		map[mailboxrpc.ServiceMethod]EnvelopeDispatcher, len(r.routes),
	)
	for k, v := range r.routes {
		m[k] = v
	}

	return m
}
