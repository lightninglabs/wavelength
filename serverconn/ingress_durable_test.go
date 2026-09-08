package serverconn_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db/actordelivery"
	"github.com/lightninglabs/wavelength/db/sqlc"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	_ "modernc.org/sqlite"
)

// openIngressStore opens real delivery persistence. A supplied PostgreSQL DSN
// must name an isolated test database; otherwise a file-backed SQLite store is
// used. Reopening the same DSN models process loss, including lost Go state.
func openIngressStore(t *testing.T,
	dsn string) (*sql.DB, actor.TxAwareDeliveryStore) {

	t.Helper()
	driver := "sqlite"
	backend := sqlc.BackendTypeSqlite
	if os.Getenv("WAVELENGTH_INGRESS_POSTGRES_DSN") != "" {
		driver = "pgx"
		backend = sqlc.BackendTypePostgres
	}
	raw, err := sql.Open(driver, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	require.NoError(t, actordelivery.RunMigrations(raw, backend))
	store, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		raw, backend, nil, nil,
	)
	require.NoError(t, err)

	return raw, store
}

// ingressTestDSN gives each SQLite test a persistent file. PostgreSQL test runs
// use a dedicated database and unique mailbox names instead.
func ingressTestDSN(t *testing.T) string {
	t.Helper()
	if dsn := os.Getenv("WAVELENGTH_INGRESS_POSTGRES_DSN"); dsn != "" {
		return dsn
	}

	return filepath.Join(t.TempDir(), "ingress.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(30000)" +
		"&_txlock=immediate"
}

// rollbackIngressStore simulates process loss after all folded writes execute
// but before their transaction commits. It uses the real store's rollback.
type rollbackIngressStore struct{ actor.TxAwareDeliveryStore }

// ExecTx aborts the fold after enqueues and checkpoint writes have succeeded.
func (s rollbackIngressStore) ExecTx(ctx context.Context, readOnly bool,
	fn actor.TxFunc) error {

	return s.TxAwareDeliveryStore.ExecTx(
		ctx, readOnly,
		func(txCtx context.Context, store actor.DeliveryStore) error {
			if err := fn(txCtx, store); err != nil {
				return err
			}

			return errors.New("injected crash before ingress " +
				"commit")
		},
	)
}

// TestIngressCursorDurableRestart drives the production ingress loop across an
// inflated response, a rolled-back fold and a fresh process opening the same
// DB. Only the final run may ACK, after inbox rows and cursor are both durable.
func TestIngressCursorDurableRestart(t *testing.T) {
	ctx := context.Background()
	dsn := ingressTestDSN(t)
	raw, store := openIngressStore(t, dsn)
	lane := t.Name() + "/" + uuid.NewString()
	envs := []*mailboxpb.Envelope{}
	for _, seq := range []uint64{4, 9} {
		envs = append(
			envs, &mailboxpb.Envelope{
				ProtocolVersion:    1,
				ArkProtocolVersion: 1,
				EventSeq:           seq,
				Rpc: &mailboxpb.RpcMeta{
					Kind:    mailboxpb.RpcMeta_KIND_EVENT,
					Service: "test",
					Method:  "event",
				},
			},
		)
	}
	cfg := serverconn.DefaultConnectorConfig()
	cfg.LocalMailboxID = lane
	cfg.RemoteMailboxID = "server"
	cfg.ArkProtocolVersion = 1
	cfg.Store = store
	cfg.RetryBaseDelay = time.Millisecond
	cfg.RetryMaxDelay = time.Millisecond
	dispatches := 0
	cfg.Dispatchers = serverconn.DispatcherMap{mailboxrpc.ServiceMethod{
		Service: "test",
		Method:  "event",
	}: func(ctx context.Context, env *mailboxpb.Envelope) error {
		dispatches++
		id := fmt.Sprintf("%s/%d", lane, env.EventSeq)

		return store.EnqueueMessage(
			ctx, actor.EnqueueParams{
				ID:          id,
				MailboxID:   lane,
				MessageType: "test",
				Payload:     []byte{byte(env.EventSeq)},
				AvailableAt: time.Now(),
				MaxAttempts: 5,
			},
		)
	}}
	next := uint64(1000)
	pulls := 0
	done := make(chan struct{})
	acked := uint64(0)
	edge := &durableIngressEdge{}
	edge.pull = func(ctx context.Context, req *mailboxpb.PullRequest) (
		*mailboxpb.PullResponse, error) {

		pulls++
		if pulls == 2 {
			close(done)
			<-ctx.Done()

			return nil, ctx.Err()
		}

		return &mailboxpb.PullResponse{
			Status: &mailboxpb.Status{
				Ok: true,
			},
			Envelopes:  envs,
			NextCursor: next,
		}, nil
	}
	edge.ack = func(_ context.Context, req *mailboxpb.AckUpToRequest) (
		*mailboxpb.AckUpToResponse, error) {

		acked = req.Cursor

		return &mailboxpb.AckUpToResponse{
			Status: &mailboxpb.Status{
				Ok: true,
			},
		}, nil
	}
	cfg.Edge = edge
	runDurableIngress(t, cfg, done)
	require.Zero(t, dispatches)
	require.Zero(t, acked)
	checkpoint, err := store.LoadCheckpoint(
		ctx, serverconn.DurableActorID(lane),
	)
	require.NoError(t, err)
	require.Nil(t, checkpoint)

	next = 10
	pulls = 0
	done = make(chan struct{})
	cfg.Store = rollbackIngressStore{TxAwareDeliveryStore: store}
	runDurableIngress(t, cfg, done)
	require.Equal(t, 2, dispatches)
	require.Zero(t, acked)
	require.NoError(t, raw.Close())

	_, store = openIngressStore(t, dsn)
	cfg.Store = store
	checkpoint, err = store.LoadCheckpoint(
		ctx, serverconn.DurableActorID(lane),
	)
	require.NoError(t, err)
	require.Nil(t, checkpoint)
	row, err := store.PeekNextMessage(ctx, lane)
	require.NoError(t, err)
	require.Nil(t, row)
	pulls = 0
	done = make(chan struct{})
	edge.ack = func(_ context.Context, req *mailboxpb.AckUpToRequest) (
		*mailboxpb.AckUpToResponse, error) {

		acked = req.Cursor
		close(done)

		return &mailboxpb.AckUpToResponse{
			Status: &mailboxpb.Status{
				Ok: true,
			},
		}, nil
	}
	// After ACK, block the next long-poll until StopIngress cancels it.
	edge.pull = func(ctx context.Context, req *mailboxpb.PullRequest) (
		*mailboxpb.PullResponse, error) {

		pulls++
		if pulls > 1 {
			<-ctx.Done()

			return nil, ctx.Err()
		}

		return &mailboxpb.PullResponse{
			Status: &mailboxpb.Status{
				Ok: true,
			},
			Envelopes:  envs,
			NextCursor: next,
		}, nil
	}
	runDurableIngress(t, cfg, done)
	require.Equal(t, uint64(10), acked)
	checkpoint, err = store.LoadCheckpoint(
		ctx, serverconn.DurableActorID(lane),
	)
	require.NoError(t, err)
	require.NotNil(t, checkpoint)
	var state serverconn.AckState
	require.NoError(t, state.Decode(bytes.NewReader(checkpoint.StateData)))
	require.Equal(t, uint64(10), state.PullCursor)
	for _, seq := range []uint64{4, 9} {
		row, err := store.PeekNextMessage(ctx, lane)
		require.NoError(t, err)
		require.NotNil(t, row)
		require.Equal(t, []byte{byte(seq)}, row.Payload)
		_, err = store.AckMessageByID(ctx, row.ID)
		require.NoError(t, err)
	}
}

// durableIngressEdge is a programmable network boundary around the real loop.
type durableIngressEdge struct {
	mailboxpb.MailboxServiceClient
	pull func(context.Context, *mailboxpb.PullRequest) (
		*mailboxpb.PullResponse, error)
	ack func(context.Context, *mailboxpb.AckUpToRequest) (
		*mailboxpb.AckUpToResponse, error)
}

// Pull returns the next scripted batch or blocks as a real long-poll would.
func (e *durableIngressEdge) Pull(ctx context.Context,
	req *mailboxpb.PullRequest, _ ...grpc.CallOption) (
	*mailboxpb.PullResponse, error) {

	return e.pull(ctx, req)
}

// AckUpTo records the remote deletion watermark chosen by production ingress.
func (e *durableIngressEdge) AckUpTo(ctx context.Context,
	req *mailboxpb.AckUpToRequest, _ ...grpc.CallOption) (
	*mailboxpb.AckUpToResponse, error) {

	return e.ack(ctx, req)
}

// runDurableIngress waits for an observed boundary before stopping both the
// ingress loop and heartbeat; test assertions then read quiescent durable
// state.
func runDurableIngress(t *testing.T, cfg serverconn.ConnectorConfig,
	done <-chan struct{}) {

	t.Helper()
	conn := serverconn.NewServerConnectionActor(cfg)
	require.NoError(t, conn.StartIngress(context.Background()))
	defer conn.StopIngress()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ingress did not reach expected boundary")
	}
}
