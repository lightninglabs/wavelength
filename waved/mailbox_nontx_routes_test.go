package waved

import (
	"reflect"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// TestNonTxRoutesResolveToMuxBridge holds the wiring invariant that the
// ingress hoist depends on and that the type system cannot express.
//
// serverconn.ConnectorConfig.NonTxRoutes declares which dispatchers answer the
// operator over the network, so the ingress loop runs them before it opens its
// write transaction. The declaration is trusted: an EnvelopeDispatcher is an
// opaque closure, so nothing downstream can check it. Marking a route whose
// dispatcher is really a durable actor Tell would hoist that enqueue out of
// the fold, where it commits ahead of the pull cursor and a crash in between
// re-enqueues it under a fresh ID that receiver-side dedup cannot collapse.
//
// buildRPCDispatchers writes both maps through one helper so that cannot
// happen by hand, but the helper is a convention. This asserts the result:
// every marked route resolves to a dispatcher, and never to one the
// EventRouter contributed.
func TestNonTxRoutesResolveToMuxBridge(t *testing.T) {
	t.Parallel()

	s := &Server{
		log: btclog.Disabled,
		cfg: &Config{
			Server: &ServerConfig{},
		},
	}

	dispatchers, nonTxRoutes := s.buildRPCDispatchers(nil)

	// The event router's own map is the set of durable Tell dispatchers.
	// Anything marked non-transactional must not be one of them.
	durable := s.buildEventRoutes().AsDispatcherMap()

	require.NotEmpty(t, nonTxRoutes)

	for route := range nonTxRoutes {
		marked, ok := dispatchers[route]
		require.Truef(
			t, ok, "route %v marked non-transactional but has "+
				"no dispatcher registered", route,
		)

		durableDispatcher, isDurable := durable[route]
		if !isDurable {
			continue
		}

		require.NotEqualf(
			t, funcPointer(durableDispatcher), funcPointer(marked),
			"route %v is marked non-transactional but resolves "+
				"to the EventRouter's durable dispatcher; "+
				"hoisting a durable Tell out of the ingress "+
				"fold breaks exactly-once local delivery",
			route,
		)
	}
}

// funcPointer returns the code pointer behind a function value, which is the
// only way to compare two closures for identity in Go.
func funcPointer(fn any) uintptr {
	return reflect.ValueOf(fn).Pointer()
}
