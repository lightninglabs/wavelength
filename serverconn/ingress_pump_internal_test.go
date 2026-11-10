package serverconn

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// TestPumpIngressBackpressure resumes at the deferred envelope after a partial
// prefix commit; the next invocation cannot redeliver the committed prefix.
func TestPumpIngressBackpressure(t *testing.T) {
	mb := newInMemoryMailbox()
	store := newWriterLockStore()
	cfg := newTestConnectorConfig(mb, store.memCheckpointStore)
	cfg.Store = store
	route := mailboxrpc.ServiceMethod{Service: "test", Method: "event"}
	deferred := true
	var delivered []uint64
	cfg.Dispatchers = DispatcherMap{
		route: func(_ context.Context, env *mailboxpb.Envelope) error {
			if deferred && env.EventSeq == 2 {
				return deferDispatch(
					route.Service, route.Method, "target",
					env.EventSeq, actor.ErrMailboxFull,
				)
			}
			delivered = append(delivered, env.EventSeq)

			return nil
		},
	}
	for range 3 {
		env := nonTxTestEnvelope(
			mailboxpb.RpcMeta_KIND_EVENT, route.Service,
			route.Method, 0,
		)
		env.Recipient = cfg.LocalMailboxID
		require.True(t, mb.send(env).Ok)
	}
	conn := NewServerConnectionActor(cfg)
	defer conn.StopIngress()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := conn.PumpIngress(ctx, 5)
	require.ErrorIs(t, err, ErrDispatchDeferred)
	require.Equal(t, uint32(1), result.Batches)
	require.Equal(t, []uint64{1}, delivered)
	state, err := conn.loadCheckpoint(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(2), state.PullCursor)
	require.Zero(t, state.AckCommittedTo)
	deferred = false
	result, err = conn.PumpIngress(ctx, 5)
	require.NoError(t, err)
	require.True(t, result.MailboxEmpty)
	require.Equal(t, []uint64{1, 2, 3}, delivered)
}

// TestPumpIngressRequiresBounds rejects unbounded invocations and stores that
// cannot atomically persist dispatch with its cursor.
func TestPumpIngressRequiresBounds(t *testing.T) {
	store := newWriterLockStore()
	cfg := newTestConnectorConfig(
		newInMemoryMailbox(), store.memCheckpointStore,
	)
	cfg.Store = store
	conn := NewServerConnectionActor(cfg)
	defer conn.StopIngress()
	_, err := conn.PumpIngress(t.Context(), 1)
	require.ErrorContains(t, err, "deadline")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err = conn.PumpIngress(ctx, 0)
	require.ErrorContains(t, err, "positive")
	cfg.PullMaxEnvelopes = 0
	_, err = NewServerConnectionActor(cfg).PumpIngress(ctx, 1)
	require.ErrorContains(t, err, "positive")
	cfg.PullMaxEnvelopes = 1
	cfg.Store = nonTxStore{DeliveryStore: store.memCheckpointStore}
	_, err = NewServerConnectionActor(cfg).PumpIngress(ctx, 1)
	require.ErrorContains(t, err, "transactional")
}

// TestIngressRegistrationFailure preserves permanent errors and bounds the
// initial heartbeat in both foreground startup and a host-driven invocation.
func TestIngressRegistrationFailure(t *testing.T) {
	cases := []struct{ mode, failure string }{
		{
			"foreground",
			"deadline",
		}, {
			"foreground",
			"version",
		},
		{
			"pump",
			"deadline",
		}, {
			"pump",
			"version",
		},
	}
	for _, tc := range cases {
		t.Run(tc.mode+"/"+tc.failure, func(t *testing.T) {
			store := newMemCheckpointStore()
			cfg := newTestConnectorConfig(
				newInMemoryMailbox(), store,
			)
			cfg.AuthSignature = &schnorr.Signature{}
			sends, pulls := 0, 0
			cfg.Edge = &mailboxClientStub{
				sendFn: func(ctx context.Context,
					_ *mailboxpb.SendRequest,
					_ ...grpc.CallOption) (
					*mailboxpb.SendResponse, error) {

					sends++
					if tc.failure == "version" {
						st := permanentStatus()

						return &mailboxpb.SendResponse{
							Status: st,
						}, nil
					}
					<-ctx.Done()

					return nil, ctx.Err()
				},
				pullFn: func(_ context.Context,
					_ *mailboxpb.PullRequest,
					_ ...grpc.CallOption) (
					*mailboxpb.PullResponse, error) {

					pulls++

					return &mailboxpb.PullResponse{}, nil
				},
			}
			conn := NewServerConnectionActor(cfg)
			defer conn.StopIngress()
			ctx, cancel := context.WithTimeout(
				t.Context(), 100*time.Millisecond,
			)
			defer cancel()
			var err error
			if tc.mode == "foreground" {
				err = conn.StartIngress(ctx)
			} else {
				_, err = conn.PumpIngress(ctx, 1)
			}
			if tc.failure == "version" {
				require.True(
					t, mailboxconn.IsPermanentVersionError(
						err,
					),
				)
			} else {
				require.ErrorIs(
					t, err, context.DeadlineExceeded,
				)
			}
			conn.StopIngress()
			require.Equal(t, 1, sends)
			require.Zero(t, pulls)
		})
	}
}
