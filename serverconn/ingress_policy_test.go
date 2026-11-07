package serverconn

import (
	"context"
	"testing"
	"time"

	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/stretchr/testify/require"
)

// TestRestrictedPumpDefersWholeBatch leaves even earlier safe responses live
// when a later unsupported request shares the pull. Foreground can retry it.
func TestRestrictedPumpDefersWholeBatch(t *testing.T) {
	mb := newInMemoryMailbox()
	store := newWriterLockStore()
	cfg := newTestConnectorConfig(mb, store.memCheckpointStore)
	cfg.Store = store
	read := mailboxrpc.ServiceMethod{Service: "index", Method: "Read"}
	unsafe := mailboxrpc.ServiceMethod{Service: "wallet", Method: "Sign"}
	dispatched := 0
	cfg.Dispatchers = DispatcherMap{unsafe: func(context.Context,
		*mailboxpb.Envelope) error {

		dispatched++

		return nil
	}}
	response := nonTxTestEnvelope(
		mailboxpb.RpcMeta_KIND_RESPONSE, read.Service, read.Method, 0,
	)
	response.Rpc.CorrelationId = "read-id"
	response.Recipient = cfg.LocalMailboxID
	require.True(t, mb.send(response).Ok)
	request := nonTxTestEnvelope(
		mailboxpb.RpcMeta_KIND_REQUEST, unsafe.Service, unsafe.Method,
		0,
	)
	request.Recipient = cfg.LocalMailboxID
	require.True(t, mb.send(request).Ok)
	conn := NewServerConnectionActor(cfg)
	defer conn.StopIngress()
	conn.responseRegistry.RegisterRequestWaiter(
		"read-id", read.Service, read.Method,
	)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := conn.PumpIngressWithPolicy(
		ctx, 1, IngressPumpPolicy{
			ReadOnlyResponses: []mailboxrpc.ServiceMethod{read},
		},
	)
	require.ErrorIs(t, err, ErrIngressNeedsForeground)
	require.Zero(t, dispatched)
	require.True(t, conn.hasResponseWaiter("read-id"))
	state, err := conn.loadCheckpoint(ctx)
	require.NoError(t, err)
	require.Zero(t, state.DispatchCommittedTo)
	_, err = conn.PumpIngress(ctx, 2)
	require.NoError(t, err)
	require.Equal(t, 1, dispatched)
}

// TestRestrictedPumpWaiterProvenance refuses manual/signing waiters even when
// a response claims an allowed read route. A real send-bound read is admitted.
func TestRestrictedPumpWaiterProvenance(t *testing.T) {
	for _, origin := range []string{"manual", "signing", "read"} {
		t.Run(origin, func(t *testing.T) {
			store := newWriterLockStore()
			cfg := newTestConnectorConfig(
				newInMemoryMailbox(), store.memCheckpointStore,
			)
			cfg.Store = store
			conn := NewServerConnectionActor(cfg)
			read := mailboxrpc.ServiceMethod{
				Service: "index",
				Method:  "Read",
			}
			switch origin {
			case "manual":
				conn.RegisterWaiter("id")

			case "signing":
				conn.responseRegistry.RegisterRequestWaiter(
					"id", "wallet", "Sign",
				)

			case "read":
				conn.responseRegistry.RegisterRequestWaiter(
					"id", read.Service, read.Method,
				)
			}
			// AwaitRPC's re-registration must preserve the send's
			// metadata.
			conn.RegisterWaiter("id")
			env := nonTxTestEnvelope(
				mailboxpb.RpcMeta_KIND_RESPONSE, read.Service,
				read.Method, 1,
			)
			env.Rpc.CorrelationId = "id"
			reads := []mailboxrpc.ServiceMethod{read}
			ctx := withIngressPolicy(
				t.Context(), IngressPumpPolicy{
					ReadOnlyResponses: reads,
				},
			)
			err := conn.validatePumpPolicy(
				ctx, []*mailboxpb.Envelope{env},
			)
			if origin == "read" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(
					t, err, ErrIngressNeedsForeground,
				)
			}
		})
	}
}

// TestRestrictedPumpNativeRequest admits a durable request-kind handoff, but
// never a mux bridge marked non-transactional on the same route.
func TestRestrictedPumpNativeRequest(t *testing.T) {
	store := newWriterLockStore()
	cfg := newTestConnectorConfig(
		newInMemoryMailbox(), store.memCheckpointStore,
	)
	cfg.Store = store
	route := mailboxrpc.ServiceMethod{Service: "durable", Method: "Accept"}
	conn := NewServerConnectionActor(cfg)
	ctx := withIngressPolicy(
		t.Context(), IngressPumpPolicy{
			DurableRoutes: []mailboxrpc.ServiceMethod{route},
		},
	)
	env := nonTxTestEnvelope(
		mailboxpb.RpcMeta_KIND_REQUEST, route.Service, route.Method, 1,
	)
	require.NoError(
		t,
		conn.validatePumpPolicy(
			ctx, []*mailboxpb.Envelope{env},
		),
	)
	conn.cfg.NonTxRoutes = RouteSet{route: {}}
	require.ErrorIs(
		t,
		conn.validatePumpPolicy(
			ctx, []*mailboxpb.Envelope{env},
		),
		ErrIngressNeedsForeground,
	)
}
