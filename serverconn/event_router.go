package serverconn

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"google.golang.org/protobuf/proto"
)

// ErrEnvelopeHandled lets an envelope route acknowledge an envelope without
// delivering an actor message. This is useful for shared RPC methods where a
// stale response can be identified as unrelated to the durable route.
var ErrEnvelopeHandled = errors.New("serverconn: envelope handled")

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

	// ResolveKey optionally maps an adapted message to a more specific
	// service key (e.g. a per-session durable mailbox). When the resolved
	// key has a live registration, the message is told straight to that
	// actor, skipping the static Key hop; otherwise dispatch falls back to
	// Key, which owns admission for actors that do not exist yet. Return
	// false to always use Key for this message.
	ResolveKey func(M) (actor.ServiceKey[M, R], bool)
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
// AddRoute is a thin wrapper around AddEnvelopeRoute that discards the
// envelope in the Adapt closure. Use AddRoute when the handler does not need
// transport metadata from the envelope.
//
// Registration is idempotent — re-registering the same (service, method) pair
// replaces the previous route.
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
		ResolveKey: cfg.ResolveKey,
	})
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

// EnvelopeRouteConfig holds parameters for registering a typed event route
// that has access to the full inbound envelope. This extends EventRouteConfig
// by passing the envelope to the Adapt closure, which allows extracting
// transport-level metadata that is not part of the proto body.
type EnvelopeRouteConfig[M actor.Message, R any] struct {
	// Service is the fully-qualified protobuf service name.
	Service string

	// Method is the protobuf method name.
	Method string

	// NewEvent must return a fresh zero-value proto.Message for
	// the expected request or response type.
	NewEvent func() proto.Message

	// Key is the ServiceKey for the target actor.
	Key actor.ServiceKey[M, R]

	// Adapt converts the deserialized proto and envelope metadata into the
	// actor message type M. Return ErrEnvelopeHandled when the envelope was
	// intentionally consumed without actor delivery.
	Adapt func(*mailboxpb.Envelope, proto.Message) (M, error)

	// ResolveKey optionally maps an adapted message to a more specific
	// service key (e.g. a per-session durable mailbox). When the resolved
	// key has a live registration, the message is told straight to that
	// actor, skipping the static Key hop; otherwise dispatch falls back to
	// Key, which owns admission for actors that do not exist yet. Return
	// false to always use Key for this message.
	ResolveKey func(M) (actor.ServiceKey[M, R], bool)
}

// AddEnvelopeRoute registers a typed event route that receives the full
// inbound envelope in its Adapt closure. This is useful when handlers need
// transport metadata like the correlation ID on a unary response envelope.
func AddEnvelopeRoute[M actor.Message, R any](r *EventRouter,
	cfg EnvelopeRouteConfig[M, R]) {

	if cfg.Service == "" {
		panic("serverconn: empty service name in EnvelopeRouteConfig")
	}
	if cfg.Method == "" {
		panic("serverconn: empty method name in EnvelopeRouteConfig")
	}
	if cfg.NewEvent == nil {
		panic("serverconn: nil NewEvent in EnvelopeRouteConfig")
	}
	if cfg.Adapt == nil {
		panic("serverconn: nil Adapt in EnvelopeRouteConfig")
	}

	system := r.system
	actorKey := cfg.Key
	resolveKey := cfg.ResolveKey

	dispatcher := func(ctx context.Context, env *mailboxpb.Envelope) error {
		if env == nil {
			return fmt.Errorf("nil envelope for %s/%s", cfg.Service,
				cfg.Method)
		}

		var event proto.Message
		if env.Body == nil {
			// Header-only unary failures are valid when the server
			// encodes the gRPC status in the envelope headers.
			// A nil body without an encoded status is malformed.
			if mailboxrpc.DecodeErrorHeaders(env.Headers) == nil {
				return fmt.Errorf("nil envelope body without "+
					"encoded error for %s/%s", cfg.Service,
					cfg.Method)
			}
		} else {
			event = cfg.NewEvent()
			if event == nil {
				return fmt.Errorf("nil event prototype for "+
					"%s/%s", cfg.Service, cfg.Method)
			}

			if err := (proto.UnmarshalOptions{
				DiscardUnknown: true,
			}).Unmarshal(
				env.Body.Value,
				event,
			); err != nil {
				return fmt.Errorf("unmarshal %s/%s event: %w",
					cfg.Service, cfg.Method, err)
			}
		}

		actorMsg, err := cfg.Adapt(env, event)
		if err != nil {
			if errors.Is(err, ErrEnvelopeHandled) {
				return nil
			}

			return fmt.Errorf("adapt %s/%s event: %w", cfg.Service,
				cfg.Method, err)
		}

		// Fast path: when the message resolves to a more specific key
		// with a live registration, tell it straight to that actor's
		// durable mailbox, skipping the coordinator hop. A miss (the
		// actor does not exist yet, or was reaped) falls back to the
		// static route key.
		if resolveKey != nil {
			if key, ok := resolveKey(actorMsg); ok {
				refs := actor.FindInReceptionist(
					system.Receptionist(), key,
				)
				if len(refs) > 0 {
					return refs[0].Tell(ctx, actorMsg)
				}
			}
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
