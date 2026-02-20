package serverconn

import (
	"context"
	"fmt"
	"sync"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"google.golang.org/protobuf/proto"
)

// InboundActorMessage is the type-constraint for actor messages that arrive
// from the server. It combines actor.Message (for dispatch via the actor
// system's Receptionist) with InboundServerMessage (for proto
// deserialization). Types that implement this constraint can be used with
// the NewEventRoute helper.
type InboundActorMessage interface {
	actor.Message
	InboundServerMessage
}

// EventRouteConfig holds parameters for registering a single typed event
// route with an EventRouter.
//
// M is the actor message type that the target actor accepts. R is the
// response type (typically unused for fire-and-forget events but required by
// the actor framework's ServiceKey generic constraint).
type EventRouteConfig[M actor.Message, R any] struct {
	// Service is the fully-qualified protobuf service name that appears
	// in the inbound envelope's RPC metadata
	// (e.g., "hellotest.v1.RoundService").
	Service string

	// Method is the protobuf method name (e.g., "RoundStarted").
	Method string

	// NewEvent must return a fresh zero-value proto.Message for the
	// expected event type. It is called once per delivered envelope to
	// provide the unmarshal target.
	NewEvent func() proto.Message

	// Key is the ServiceKey for the target durable actor. The router
	// calls key.Ref(system).Tell(ctx, msg) for each dispatched event,
	// which persists the message to the actor's durable mailbox before
	// returning nil.
	Key actor.ServiceKey[M, R]

	// Adapt converts the deserialized proto.Message to the actor message
	// type M. Return an error to reject envelopes whose body cannot be
	// converted.
	Adapt func(proto.Message) (M, error)
}

// EventRouter maps inbound KIND_REQUEST and KIND_EVENT envelope routes to
// typed durable actor mailboxes via ServiceKey.
//
// EventRouter resolves target actors through the actor system's Receptionist,
// guaranteeing durable delivery before returning from each dispatch call.
//
// At wiring time, callers call AddRoute for each (service, method) pair they
// want to handle, then pass AsDispatcherMap() to ConnectorConfig.Dispatchers.
type EventRouter struct {
	mu     sync.RWMutex
	system actor.SystemContext
	routes map[mailboxrpc.ServiceMethod]EnvelopeDispatcher
}

// NewEventRouter creates an empty EventRouter backed by the given actor system.
// The system is used to resolve ServiceKeys to actor references at dispatch
// time.
func NewEventRouter(system actor.SystemContext) *EventRouter {
	return &EventRouter{
		system: system,
		routes: make(map[mailboxrpc.ServiceMethod]EnvelopeDispatcher),
	}
}

// AddRoute registers a typed event route with the router. The generic
// parameters [M, R] must match the ServiceKey's type parameters.
//
// AddRoute is a package-level generic function rather than a method because Go
// does not allow methods with type parameters on non-generic types.
//
// Registration is idempotent — re-registering the same (service, method) pair
// replaces the previous route.
func AddRoute[M actor.Message, R any](r *EventRouter,
	cfg EventRouteConfig[M, R]) {

	if cfg.Service == "" {
		panic("serverconn: empty service name in EventRouteConfig")
	}
	if cfg.Method == "" {
		panic("serverconn: empty method name in EventRouteConfig")
	}
	if cfg.NewEvent == nil {
		panic("serverconn: nil NewEvent in EventRouteConfig")
	}
	if cfg.Adapt == nil {
		panic("serverconn: nil Adapt in EventRouteConfig")
	}

	// Capture config fields and system in a closure to produce the
	// type-erased EnvelopeDispatcher. The closure owns the full dispatch
	// chain: deserialize → adapt → Tell (persist to durable mailbox).
	system := r.system
	actorKey := cfg.Key

	dispatcher := func(ctx context.Context,
		env *mailboxpb.Envelope) error {

		if env == nil || env.Body == nil {
			return fmt.Errorf("nil envelope or body for %s/%s",
				cfg.Service, cfg.Method)
		}

		// Deserialize the envelope body. The body is an anypb.Any;
		// its Value field carries the raw proto bytes of the inner
		// event message. We unmarshal those bytes directly into the
		// registered event type for forward-compatible decoding.
		event := cfg.NewEvent()
		if event == nil {
			return fmt.Errorf("nil event prototype for %s/%s",
				cfg.Service, cfg.Method)
		}

		if err := (proto.UnmarshalOptions{
			DiscardUnknown: true,
		}).Unmarshal(env.Body.Value, event); err != nil {
			return fmt.Errorf("unmarshal %s/%s event: %w",
				cfg.Service, cfg.Method, err)
		}

		// Convert the proto event to the actor's message type.
		actorMsg, err := cfg.Adapt(event)
		if err != nil {
			return fmt.Errorf("adapt %s/%s event: %w",
				cfg.Service, cfg.Method, err)
		}

		// Dispatch to the target actor via the service key. Ref
		// returns a virtual router that load-balances across all
		// registered actors for this key. Tell persists the message
		// to the actor's durable mailbox before returning, satisfying
		// the EnvelopeDispatcher contract of committed delivery.
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

// InboundEventRouteConfig holds parameters for registering an event route
// where the actor message type implements InboundActorMessage. NewEventRoute
// auto-generates the Adapt closure from M.FromProto, so callers don't need
// to write one manually.
type InboundEventRouteConfig[M InboundActorMessage, R any] struct {
	// Service is the fully-qualified protobuf service name
	// (e.g., "hellotest.v1.HelloService").
	Service string

	// Method is the protobuf method name (e.g., "HelloStarted").
	Method string

	// Key is the ServiceKey for the target durable actor.
	Key actor.ServiceKey[M, R]

	// NewEvent must return a fresh zero-value proto.Message for the
	// expected event type.
	NewEvent func() proto.Message

	// NewMsg must return a non-nil zero-value M. FromProto is called
	// on the returned value to populate it from the deserialized event.
	NewMsg func() M
}

// NewEventRoute registers a typed event route for actor message types that
// implement InboundActorMessage. It auto-generates the Adapt closure from
// M.FromProto, eliminating boilerplate for the common case where the actor
// message knows how to deserialize itself from proto.
func NewEventRoute[M InboundActorMessage, R any](r *EventRouter,
	cfg InboundEventRouteConfig[M, R]) {

	if cfg.NewMsg == nil {
		panic("serverconn: nil NewMsg in InboundEventRouteConfig")
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

// DispatcherMap maps envelope routing keys to their dispatch closures. It
// is the type returned by AsDispatcherMap and consumed by
// ConnectorConfig.Dispatchers.
type DispatcherMap = map[mailboxrpc.ServiceMethod]EnvelopeDispatcher

// AsDispatcherMap returns a shallow copy of the registered routes as a
// DispatcherMap suitable for use as ConnectorConfig.Dispatchers.
//
// The returned map is safe to read concurrently. Callers should call
// AsDispatcherMap after all routes have been registered, before
// constructing the ConnectorConfig.
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
