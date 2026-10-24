package serverconn_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/stretchr/testify/require"
)

// pumpHarness supplies real file-backed persistence and a scripted remote
// mailbox. No actor workers run, so an empty pull can coexist with local work.
type pumpHarness struct {
	cfg        serverconn.ConnectorConfig
	edge       *durableIngressEdge
	raw        *sql.DB
	store      actor.TxAwareDeliveryStore
	dsn        string
	envelopes  []*mailboxpb.Envelope
	pulls      atomic.Int32
	acks       atomic.Int32
	dispatches atomic.Int32
}

// newPumpHarness creates one isolated mailbox with sparse sequence numbers.
func newPumpHarness(t *testing.T) *pumpHarness {
	t.Helper()
	h := &pumpHarness{dsn: ingressTestDSN(t)}
	h.raw, h.store = openIngressStore(t, h.dsn)
	h.cfg = serverconn.DefaultConnectorConfig()
	h.cfg.ArkProtocolVersion = 1
	h.cfg.LocalMailboxID = t.Name() + uuid.NewString()
	h.cfg.RemoteMailboxID = "server"
	h.cfg.Store = h.store
	h.cfg.PullMaxEnvelopes = 1
	route := mailboxrpc.ServiceMethod{Service: "test", Method: "event"}
	h.cfg.Dispatchers = serverconn.DispatcherMap{
		route: func(ctx context.Context,
			env *mailboxpb.Envelope) error {

			h.dispatches.Add(1)

			return h.store.EnqueueMessage(ctx, actor.EnqueueParams{
				ID: fmt.Sprintf("%s/%d", h.cfg.LocalMailboxID,
					env.EventSeq),
				MailboxID:   h.cfg.LocalMailboxID,
				MessageType: "test",
				Payload:     []byte{byte(env.EventSeq)},
				AvailableAt: time.Now(),
				MaxAttempts: 5,
			})
		},
	}
	for _, seq := range []uint64{4, 9, 12} {
		h.envelopes = append(h.envelopes, &mailboxpb.Envelope{
			ProtocolVersion:    1,
			ArkProtocolVersion: 1,
			EventSeq:           seq,
			Rpc: &mailboxpb.RpcMeta{
				Kind:    mailboxpb.RpcMeta_KIND_EVENT,
				Service: route.Service,
				Method:  route.Method,
			},
		})
	}
	h.edge = &durableIngressEdge{}
	h.edge.pull = func(_ context.Context, req *mailboxpb.PullRequest) (
		*mailboxpb.PullResponse, error) {

		h.pulls.Add(1)
		require.Zero(t, req.WaitTimeoutMs)
		resp := &mailboxpb.PullResponse{NextCursor: req.Cursor}
		for _, env := range h.envelopes {
			if env.EventSeq < req.Cursor {
				continue
			}
			resp.Envelopes = append(resp.Envelopes, env)
			resp.NextCursor = env.EventSeq + 1
			if len(resp.Envelopes) == int(req.MaxEnvelopes) {
				break
			}
		}

		return resp, nil
	}
	h.edge.ack = func(ctx context.Context, req *mailboxpb.AckUpToRequest) (
		*mailboxpb.AckUpToResponse, error) {

		h.acks.Add(1)
		state := h.checkpoint(t, ctx)
		require.GreaterOrEqual(t, state.DispatchCommittedTo, req.Cursor)
		row, err := h.store.PeekNextMessage(ctx, h.cfg.LocalMailboxID)
		require.NoError(t, err)
		require.NotNil(t, row, "ACK requires a committed inbox row")

		return &mailboxpb.AckUpToResponse{}, nil
	}
	h.cfg.Edge = h.edge

	return h
}

// checkpoint decodes the durable cursor rather than trusting pump counters.
func (h *pumpHarness) checkpoint(t *testing.T,
	ctx context.Context) serverconn.AckState {

	t.Helper()
	cp, err := h.store.LoadCheckpoint(
		ctx, serverconn.DurableActorID(h.cfg.LocalMailboxID),
	)
	require.NoError(t, err)
	var state serverconn.AckState
	if cp != nil {
		require.NoError(t, state.Decode(bytes.NewReader(cp.StateData)))
	}

	return state
}

// reopen discards the SQL connection and returns a fresh connector, modeling
// loss of all connector-local state between wakes.
func (h *pumpHarness) reopen(t *testing.T) *serverconn.ServerConnectionActor {
	t.Helper()
	require.NoError(t, h.raw.Close())
	h.raw, h.store = openIngressStore(t, h.dsn)
	h.cfg.Store = h.store

	return serverconn.NewServerConnectionActor(h.cfg)
}

// pumpContext gives each invocation a bounded test lifetime.
func pumpContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	return ctx
}

// TestPumpIngressBatchLimitAndReopen proves bounded work, exclusive cursor
// continuation, and the distinction between an empty remote mailbox and
// durable local work still waiting for actors.
func TestPumpIngressBatchLimitAndReopen(t *testing.T) {
	h := newPumpHarness(t)
	runtime, err := serverconn.NewRuntime(h.cfg)
	require.NoError(t, err)
	result, err := runtime.PumpIngress(pumpContext(t), 1)
	require.NoError(t, err)
	require.Equal(t, uint32(1), result.Batches)
	require.False(t, result.MailboxEmpty)
	require.EqualValues(t, 1, h.pulls.Load())
	require.EqualValues(t, 1, h.acks.Load())
	runtime.Stop()

	conn := h.reopen(t)
	defer conn.StopIngress()
	result, err = conn.PumpIngress(pumpContext(t), 3)
	require.NoError(t, err)
	require.Equal(t, uint32(2), result.Batches)
	require.True(t, result.MailboxEmpty)
	require.EqualValues(t, 3, h.dispatches.Load())
	require.Equal(
		t, uint64(13), h.checkpoint(t, t.Context()).AckCommittedTo,
	)
	result, err = conn.PumpIngress(pumpContext(t), 1)
	require.NoError(t, err)
	require.True(t, result.MailboxEmpty)
	require.Zero(t, result.Batches)
	require.EqualValues(t, 3, h.dispatches.Load())

	for _, seq := range []byte{4, 9, 12} {
		row, err := h.store.PeekNextMessage(
			t.Context(), h.cfg.LocalMailboxID,
		)
		require.NoError(t, err)
		require.NotNil(t, row)
		require.Equal(t, []byte{seq}, row.Payload)
		_, err = h.store.AckMessageByID(t.Context(), row.ID)
		require.NoError(t, err)
	}
	row, err := h.store.PeekNextMessage(t.Context(), h.cfg.LocalMailboxID)
	require.NoError(t, err)
	require.Nil(t, row)
}

// TestPumpIngressDeadlineAfterCommit reopens a committed batch whose remote
// ACK timed out. The next wake ACKs it before pulling without redelivering it.
func TestPumpIngressDeadlineAfterCommit(t *testing.T) {
	h := newPumpHarness(t)
	ack := h.edge.ack
	h.edge.ack = func(ctx context.Context, _ *mailboxpb.AckUpToRequest) (
		*mailboxpb.AckUpToResponse, error) {

		<-ctx.Done()

		return nil, ctx.Err()
	}
	conn := serverconn.NewServerConnectionActor(h.cfg)
	ctx, cancel := context.WithTimeout(
		context.Background(), 100*time.Millisecond,
	)
	defer cancel()
	result, err := conn.PumpIngress(ctx, 1)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, uint32(1), result.Batches)
	conn.StopIngress()
	h.edge.ack = ack
	conn = h.reopen(t)
	defer conn.StopIngress()
	state := h.checkpoint(t, t.Context())
	require.Equal(t, uint64(5), state.DispatchCommittedTo)
	require.Zero(t, state.AckCommittedTo)
	result, err = conn.PumpIngress(pumpContext(t), 4)
	require.NoError(t, err)
	require.True(t, result.MailboxEmpty)
	require.Equal(t, uint32(2), result.Batches)
	require.EqualValues(t, 3, h.dispatches.Load())
}

// TestPumpIngressRollbackNeverAcks verifies that a crash before the SQL commit
// leaves neither inbox rows nor an advanced cursor for a later wake to ACK.
func TestPumpIngressRollbackNeverAcks(t *testing.T) {
	h := newPumpHarness(t)
	h.cfg.Store = rollbackIngressStore{TxAwareDeliveryStore: h.store}
	conn := serverconn.NewServerConnectionActor(h.cfg)
	result, err := conn.PumpIngress(pumpContext(t), 2)
	require.ErrorContains(t, err, "injected crash")
	require.Equal(t, uint32(1), result.Batches)
	require.Zero(t, h.acks.Load())
	conn.StopIngress()
	conn = h.reopen(t)
	defer conn.StopIngress()
	require.Zero(t, h.checkpoint(t, t.Context()).PullCursor)
	row, err := h.store.PeekNextMessage(t.Context(), h.cfg.LocalMailboxID)
	require.NoError(t, err)
	require.Nil(t, row)
	result, err = conn.PumpIngress(pumpContext(t), 4)
	require.NoError(t, err)
	require.True(t, result.MailboxEmpty)
	require.EqualValues(t, 3, h.acks.Load())
}

// TestPumpIngressDeadlineDuringWork verifies cancellation before work, during
// a pull, and inside a durable dispatch transaction, with no ACK in each case.
func TestPumpIngressDeadlineDuringWork(t *testing.T) {
	for _, phase := range []string{"expired", "pull", "dispatch"} {
		t.Run(phase, func(t *testing.T) {
			h := newPumpHarness(t)
			if phase == "pull" {
				h.edge.pull = func(ctx context.Context,
					_ *mailboxpb.PullRequest) (
					*mailboxpb.PullResponse, error) {

					<-ctx.Done()

					return nil, ctx.Err()
				}
			}
			if phase == "dispatch" {
				for route, dispatch := range h.cfg.Dispatchers {
					h.cfg.Dispatchers[route] = func(
						ctx context.Context,
						env *mailboxpb.Envelope) error {

						if err := dispatch(
							ctx, env,
						); err != nil {
							return err
						}
						<-ctx.Done()

						return ctx.Err()
					}
				}
			}
			deadline := time.Now().Add(100 * time.Millisecond)
			if phase == "expired" {
				deadline = time.Now().Add(-time.Second)
			}
			ctx, cancel := context.WithDeadline(
				context.Background(), deadline,
			)
			defer cancel()
			conn := serverconn.NewServerConnectionActor(h.cfg)
			_, err := conn.PumpIngress(ctx, 1)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			conn.StopIngress()
			require.Zero(t, h.acks.Load())
			conn = h.reopen(t)
			defer conn.StopIngress()
			require.Zero(t, h.checkpoint(t, t.Context()).PullCursor)
			row, err := h.store.PeekNextMessage(
				t.Context(), h.cfg.LocalMailboxID,
			)
			require.NoError(t, err)
			require.Nil(t, row)
			if phase == "expired" {
				require.Zero(t, h.pulls.Load())
			}
		})
	}
}

// TestPumpIngressFailureYields checks that transient failures do not consume a
// wake in backoff, and malformed or oversized batches cannot advance the ACK.
func TestPumpIngressFailureYields(t *testing.T) {
	for _, failure := range []string{"transport", "oversized", "version"} {
		t.Run(failure, func(t *testing.T) {
			h := newPumpHarness(t)
			pull := h.edge.pull
			h.edge.pull = func(ctx context.Context,
				req *mailboxpb.PullRequest) (
				*mailboxpb.PullResponse, error) {

				switch failure {
				case "transport":
					return nil, errors.New("offline")

				case "oversized":
					return &mailboxpb.PullResponse{
						Envelopes:  h.envelopes,
						NextCursor: 13,
					}, nil

				default:
					h.envelopes[0].ArkProtocolVersion = 99

					return pull(ctx, req)
				}
			}
			conn := serverconn.NewServerConnectionActor(h.cfg)
			defer conn.StopIngress()
			_, err := conn.PumpIngress(pumpContext(t), 2)
			require.Error(t, err)
			require.Zero(t, h.dispatches.Load())
			require.Zero(t, h.acks.Load())
			require.Zero(t, h.checkpoint(t, t.Context()).PullCursor)
			if failure == "version" {
				_, nextErr := conn.PumpIngress(
					pumpContext(t), 1,
				)
				require.Equal(t, err, nextErr)
				require.EqualValues(t, 1, h.pulls.Load())
			}
		})
	}
}

// TestPumpIngressExclusiveOwner exercises duplicate wakes, foreground mode,
// pause/resume and terminal stop through the public runtime interface.
func TestPumpIngressExclusiveOwner(t *testing.T) {
	h := newPumpHarness(t)
	entered := make(chan struct{}, 2)
	h.edge.pull = func(ctx context.Context, _ *mailboxpb.PullRequest) (
		*mailboxpb.PullResponse, error) {

		entered <- struct{}{}
		<-ctx.Done()

		return nil, ctx.Err()
	}
	runtime, err := serverconn.NewRuntime(h.cfg)
	require.NoError(t, err)
	defer runtime.Stop()
	done := make(chan error, 1)
	go func() {
		_, err := runtime.PumpIngress(pumpContext(t), 1)
		done <- err
	}()
	awaitPumpSignal(t, entered)
	_, err = runtime.PumpIngress(pumpContext(t), 1)
	require.ErrorIs(t, err, serverconn.ErrIngressBusy)
	require.ErrorIs(
		t,
		runtime.StartIngress(
			t.Context(),
		),
		serverconn.ErrIngressBusy,
	)
	runtime.PauseIngress()
	require.ErrorIs(t, <-done, context.Canceled)
	require.NoError(t, runtime.StartIngress(t.Context()))
	awaitPumpSignal(t, entered)
	_, err = runtime.PumpIngress(pumpContext(t), 1)
	require.ErrorIs(t, err, serverconn.ErrIngressBusy)
	runtime.PauseIngress()
	h.edge.pull = func(_ context.Context, req *mailboxpb.PullRequest) (
		*mailboxpb.PullResponse, error) {

		return &mailboxpb.PullResponse{NextCursor: req.Cursor}, nil
	}
	result, err := runtime.PumpIngress(pumpContext(t), 1)
	require.NoError(t, err)
	require.True(t, result.MailboxEmpty)
	runtime.Stop()
	_, err = runtime.PumpIngress(pumpContext(t), 1)
	require.ErrorIs(t, err, serverconn.ErrIngressStopped)
	require.ErrorIs(
		t,
		runtime.StartIngress(
			t.Context(),
		),
		serverconn.ErrIngressStopped,
	)
}

// awaitPumpSignal bounds test coordination so lifecycle bugs fail visibly.
func awaitPumpSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("ingress did not reach the expected boundary")
	}
}

// blockedCheckpointStore exposes startup cancellation and its join boundary.
type blockedCheckpointStore struct {
	actor.TxAwareDeliveryStore
	entered chan struct{}
	release chan struct{}
}

// LoadCheckpoint holds the lease even after cancellation until cleanup ends.
func (s *blockedCheckpointStore) LoadCheckpoint(ctx context.Context, _ string) (
	*actor.Checkpoint, error) {

	close(s.entered)
	<-ctx.Done()
	<-s.release

	return nil, ctx.Err()
}

// TestIngressStopJoinsCheckpointLoad proves stop covers startup and all stop
// callers wait for the same active owner, even before any worker is launched.
func TestIngressStopJoinsCheckpointLoad(t *testing.T) {
	for _, mode := range []string{"foreground", "pump"} {
		t.Run(mode, func(t *testing.T) {
			h := newPumpHarness(t)
			store := &blockedCheckpointStore{
				TxAwareDeliveryStore: h.store,
				entered:              make(chan struct{}),
				release:              make(chan struct{}),
			}
			h.cfg.Store = store
			conn := serverconn.NewServerConnectionActor(h.cfg)
			workDone := make(chan error, 1)
			ctx := pumpContext(t)
			go func() {
				if mode == "foreground" {
					workDone <- conn.StartIngress(ctx)

					return
				}
				_, err := conn.PumpIngress(ctx, 1)
				workDone <- err
			}()
			awaitPumpSignal(t, store.entered)
			stopped := make(chan struct{}, 2)
			for range 2 {
				go func() {
					conn.StopIngress()
					stopped <- struct{}{}
				}()
			}
			select {
			case <-stopped:
				t.Fatal("stop returned before startup cleanup")

			case <-time.After(20 * time.Millisecond):
			}
			close(store.release)
			awaitPumpSignal(t, stopped)
			awaitPumpSignal(t, stopped)
			require.ErrorIs(t, <-workDone, context.Canceled)
			require.Zero(t, h.pulls.Load())
		})
	}
}

// failAckCheckpointStore interrupts only the checkpoint after a remote ACK;
// folded dispatch transactions still use the real transaction-scoped store.
type failAckCheckpointStore struct{ actor.TxAwareDeliveryStore }

// SaveCheckpoint simulates local persistence failure after remote deletion.
func (s failAckCheckpointStore) SaveCheckpoint(ctx context.Context,
	params actor.CheckpointParams) error {

	var state serverconn.AckState
	if err := state.Decode(bytes.NewReader(params.StateData)); err != nil {
		return err
	}
	if state.AckCommittedTo > 0 {
		return errors.New("injected ACK checkpoint failure")
	}

	return s.TxAwareDeliveryStore.SaveCheckpoint(ctx, params)
}

// TestPumpIngressRepeatsAckAfterCheckpointFailure proves a successful remote
// ACK followed by a local failure repeats only the ACK after reopening.
func TestPumpIngressRepeatsAckAfterCheckpointFailure(t *testing.T) {
	h := newPumpHarness(t)
	h.cfg.Store = failAckCheckpointStore{TxAwareDeliveryStore: h.store}
	conn := serverconn.NewServerConnectionActor(h.cfg)
	_, err := conn.PumpIngress(pumpContext(t), 1)
	require.ErrorContains(t, err, "injected ACK checkpoint failure")
	require.EqualValues(t, 1, h.acks.Load())
	conn.StopIngress()
	conn = h.reopen(t)
	defer conn.StopIngress()
	state := h.checkpoint(t, t.Context())
	require.Equal(t, uint64(5), state.PullCursor)
	require.Zero(t, state.AckCommittedTo)
	result, err := conn.PumpIngress(pumpContext(t), 4)
	require.NoError(t, err)
	require.True(t, result.MailboxEmpty)
	require.EqualValues(t, 4, h.acks.Load())
	require.EqualValues(t, 3, h.dispatches.Load())
}
