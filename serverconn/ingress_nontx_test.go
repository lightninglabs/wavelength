package serverconn

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/stretchr/testify/require"
)

const (
	// nonTxService and nonTxMethod name the mux-bridged route whose
	// dispatcher answers over the network instead of enqueuing durably.
	nonTxService = "waverpc.DaemonService"
	nonTxMethod  = "GetInfo"

	// durableService and durableMethod name a route whose dispatcher is a
	// durable actor Tell and therefore must stay inside the fold.
	durableService = "hellotest.v1.RoundService"
	durableMethod  = "RoundStarted"
)

// writerLockStore models the single global writer that both production
// backends impose on an open write transaction: SQLite is opened with
// _txlock=immediate so a write transaction takes the one database-wide write
// lock, and db.BaseDB.BeginTx pins Postgres to SERIALIZABLE so the snapshot
// conflict window stays open for the transaction's whole lifetime. The
// in-memory checkpoint store's own ExecTx runs the callback with nothing held,
// so it cannot observe the stall this models.
//
// The lock is what the test asserts against: any dispatch that runs with it
// held is dispatch that would have blocked every other writer in the process
// for its own duration.
type writerLockStore struct {
	*memCheckpointStore

	writer sync.Mutex

	mu sync.Mutex

	// events records the observable steps in the order they happened, so
	// the test can pin dispatch-before-commit rather than only checking
	// that both occurred.
	events []string
}

// newWriterLockStore wraps an in-memory checkpoint store with the modeled
// writer lock.
func newWriterLockStore() *writerLockStore {
	return &writerLockStore{
		memCheckpointStore: newMemCheckpointStore(),
	}
}

// ExecTx takes the modeled writer lock for the whole transaction, mirroring
// production, and releases it on commit or rollback.
func (s *writerLockStore) ExecTx(ctx context.Context, _ bool,
	fn actor.TxFunc) error {

	s.writer.Lock()
	defer s.writer.Unlock()

	s.record("tx-open")

	err := fn(ctx, s)

	s.record("tx-close")

	return err
}

// SaveCheckpoint records the cursor commit before delegating, so the test can
// order it against the dispatches.
func (s *writerLockStore) SaveCheckpoint(ctx context.Context,
	params actor.CheckpointParams) error {

	s.record("checkpoint")

	return s.memCheckpointStore.SaveCheckpoint(ctx, params)
}

// writerHeld reports whether the modeled writer lock is currently held. Go
// mutexes are not reentrant, so this correctly reports true when called from
// inside the transaction's own goroutine, which is exactly where a folded
// dispatcher runs.
func (s *writerLockStore) writerHeld() bool {
	if !s.writer.TryLock() {
		return true
	}

	s.writer.Unlock()

	return false
}

// record appends a step to the observed event log.
func (s *writerLockStore) record(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.events = append(s.events, event)
}

// observed returns a copy of the observed event log.
func (s *writerLockStore) observed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.events...)
}

var _ actor.TxAwareDeliveryStore = (*writerLockStore)(nil)

// nonTxTestEnvelope builds a version-stamped envelope for the ingress tests
// below, matching the version pair that newTestConnectorConfig binds.
func nonTxTestEnvelope(kind mailboxpb.RpcMeta_Kind, service, method string,
	seq uint64) *mailboxpb.Envelope {

	return &mailboxpb.Envelope{
		ProtocolVersion:    1,
		ArkProtocolVersion: 1,
		EventSeq:           seq,
		Rpc: &mailboxpb.RpcMeta{
			Kind:    kind,
			Service: service,
			Method:  method,
		},
	}
}

// nonTxProbe is a connector plus the writer-lock observations its dispatchers
// made, one entry per invocation in call order.
type nonTxProbe struct {
	conn *ServerConnectionActor

	// requestHeld records, for each hoisted request dispatch, whether the
	// modeled writer lock was held at the time.
	requestHeld []bool

	// durableHeld records the same for each durable dispatch.
	durableHeld []bool
}

// newNonTxProbe builds a connector whose dispatch table has one mux-bridged
// route marked non-transactional and one durable route that is not. Both
// dispatchers report whether the modeled writer lock was held when they ran,
// and both append to the store's event log so the test can order them.
func newNonTxProbe(t *testing.T, store *writerLockStore,
	requestErr error) *nonTxProbe {

	t.Helper()

	probe := &nonTxProbe{}

	nonTxRoute := mailboxrpc.ServiceMethod{
		Service: nonTxService,
		Method:  nonTxMethod,
	}
	durableRoute := mailboxrpc.ServiceMethod{
		Service: durableService,
		Method:  durableMethod,
	}

	cfg := newTestConnectorConfig(newInMemoryMailbox(), nil)
	cfg.MailboxProtocolVersion = 1
	cfg.ArkProtocolVersion = 1
	cfg.Store = store
	cfg.Dispatchers = DispatcherMap{
		nonTxRoute: func(context.Context, *mailboxpb.Envelope) error {
			store.record("serve-request")
			probe.requestHeld = append(
				probe.requestHeld, store.writerHeld(),
			)

			return requestErr
		},
		durableRoute: func(context.Context, *mailboxpb.Envelope) error {
			store.record("enqueue-durable")
			probe.durableHeld = append(
				probe.durableHeld, store.writerHeld(),
			)

			return nil
		},
	}
	cfg.NonTxRoutes = RouteSet{
		nonTxRoute: {},
	}

	probe.conn = NewServerConnectionActor(cfg)

	return probe
}

// TestRunFoldedDispatchServesRequestsOutsideTx is the regression test for the
// ingress loop holding the database writer across a network round trip. A
// mux-bridged KIND_REQUEST dispatcher serves the request and sends the
// KIND_RESPONSE back over the edge, so running it inside the folded write
// transaction pins the SQLite global writer lock, or a SERIALIZABLE Postgres
// snapshot, for the full duration of a round trip to the operator.
//
// The test asserts the property directly rather than by timing: the hoisted
// dispatcher must observe the writer lock free, while the durable dispatcher
// in the same batch must still observe it held, since its enqueue has to
// commit atomically with the cursor.
func TestRunFoldedDispatchServesRequestsOutsideTx(t *testing.T) {
	t.Parallel()

	store := newWriterLockStore()
	probe := newNonTxProbe(t, store, nil)

	envelopes := []*mailboxpb.Envelope{
		nonTxTestEnvelope(
			mailboxpb.RpcMeta_KIND_REQUEST, nonTxService,
			nonTxMethod, 1,
		),
		nonTxTestEnvelope(
			mailboxpb.RpcMeta_KIND_EVENT, durableService,
			durableMethod, 2,
		),
	}

	newState, err := probe.conn.runFoldedDispatch(
		t.Context(), store, envelopes, 3, AckState{},
	)
	require.NoError(t, err)

	// The inbound request was served with no write transaction open.
	require.Equal(t, []bool{false}, probe.requestHeld)

	// The durable enqueue still runs under the transaction, so it commits
	// with the cursor and a rollback takes both.
	require.Equal(t, []bool{true}, probe.durableHeld)

	// The cursor advanced exactly once, covering the whole batch.
	require.Equal(t, uint64(3), newState.PullCursor)
	require.Equal(t, uint64(3), newState.DispatchCommittedTo)

	// Serving the request precedes the commit. At-least-once needs that
	// ordering: committing first would let a crash in the window advance
	// the cursor past a request that was never answered.
	wantOrder := []string{
		"serve-request", "tx-open", "enqueue-durable", "checkpoint",
		"tx-close",
	}
	require.Equal(t, wantOrder, store.observed())
}

// TestRunFoldedDispatchRequestFailureHoldsCursor verifies that a failure while
// serving a hoisted request leaves the cursor untouched and never opens the
// transaction, so the batch is re-pulled whole rather than partially acked.
func TestRunFoldedDispatchRequestFailureHoldsCursor(t *testing.T) {
	t.Parallel()

	serveErr := errors.New("send RPC response: transport closed")

	store := newWriterLockStore()
	probe := newNonTxProbe(t, store, serveErr)

	envelopes := []*mailboxpb.Envelope{
		nonTxTestEnvelope(
			mailboxpb.RpcMeta_KIND_REQUEST, nonTxService,
			nonTxMethod, 1,
		),
		nonTxTestEnvelope(
			mailboxpb.RpcMeta_KIND_EVENT, durableService,
			durableMethod, 2,
		),
	}

	state := AckState{PullCursor: 1}
	newState, err := probe.conn.runFoldedDispatch(
		t.Context(), store, envelopes, 3, state,
	)
	require.ErrorIs(t, err, serveErr)

	// The returned state is the caller's, unchanged, so the ingress loop
	// backs off and re-pulls from the same cursor.
	require.Equal(t, state, newState)

	// The transaction never opened, so the durable half of the batch was
	// not dispatched either.
	require.Empty(t, probe.durableHeld)
	require.Equal(t, []string{"serve-request"}, store.observed())
}

// TestRunFoldedDispatchValidatesBeforeAnyDelivery pins that a permanently
// incompatible envelope anywhere in the batch stops the loop before any of the
// batch is acted on. Without the up-front sweep, an earlier hoisted request
// would already have been served and answered over the wire by the time the
// later mismatch aborted the transaction, and that send cannot be taken back.
func TestRunFoldedDispatchValidatesBeforeAnyDelivery(t *testing.T) {
	t.Parallel()

	store := newWriterLockStore()
	probe := newNonTxProbe(t, store, nil)

	mismatched := nonTxTestEnvelope(
		mailboxpb.RpcMeta_KIND_EVENT, durableService, durableMethod, 2,
	)
	mismatched.ArkProtocolVersion = 99

	envelopes := []*mailboxpb.Envelope{
		nonTxTestEnvelope(
			mailboxpb.RpcMeta_KIND_REQUEST, nonTxService,
			nonTxMethod, 1,
		),
		mismatched,
	}

	state := AckState{PullCursor: 1}
	newState, err := probe.conn.runFoldedDispatch(
		t.Context(), store, envelopes, 3, state,
	)
	require.Error(t, err)
	require.True(t, probe.conn.checkPermanentStatus(t.Context(), err))
	require.Equal(t, state, newState)

	// Nothing ran: not the hoisted request, not the durable dispatch, not
	// the transaction.
	require.Empty(t, probe.requestHeld)
	require.Empty(t, probe.durableHeld)
	require.Empty(t, store.observed())
}

// TestDispatchBatchSkipsMislabeledNonTxKinds closes the hole the kind guard
// would otherwise leave open. The hoist gate only takes a KIND_REQUEST out of
// the fold, so an envelope stamped KIND_EVENT, or a KIND_RESPONSE whose waiter
// is gone, still resolves to the marked route's dispatcher inside the
// transaction. That dispatcher is the mux bridge, which never looks at the
// kind: it would serve the envelope as a request and answer over the wire with
// the writer lock held, which is the same stall this split removes for the
// well-formed case. The sender controls the kind, so leaving that path live
// would leave the fix half applied.
func TestDispatchBatchSkipsMislabeledNonTxKinds(t *testing.T) {
	t.Parallel()

	store := newWriterLockStore()
	probe := newNonTxProbe(t, store, nil)

	// A KIND_RESPONSE on the marked route with no waiter registered falls
	// through to the same dispatch table lookup as an event does.
	orphanResp := nonTxTestEnvelope(
		mailboxpb.RpcMeta_KIND_RESPONSE, nonTxService, nonTxMethod, 2,
	)
	orphanResp.Rpc.CorrelationId = "no-such-waiter"

	envelopes := []*mailboxpb.Envelope{
		nonTxTestEnvelope(
			mailboxpb.RpcMeta_KIND_EVENT, nonTxService, nonTxMethod,
			1,
		),
		orphanResp,
		nonTxTestEnvelope(
			mailboxpb.RpcMeta_KIND_EVENT, durableService,
			durableMethod, 3,
		),
	}

	newState, err := probe.conn.runFoldedDispatch(
		t.Context(), store, envelopes, 4, AckState{},
	)
	require.NoError(t, err)

	// Neither mislabeled envelope reached the mux bridge, so no round trip
	// happened under the transaction.
	require.Empty(t, probe.requestHeld)
	require.NotContains(t, store.observed(), "serve-request")

	// The rest of the batch still ran and the cursor still advanced. A
	// mislabeled envelope must not wedge the loop: a dispatch error here
	// is not permanent, so returning one would back off and re-pull the
	// same envelope forever.
	require.Equal(t, []bool{true}, probe.durableHeld)
	require.Equal(t, uint64(4), newState.PullCursor)
	require.Equal(t, uint64(4), newState.DispatchCommittedTo)
}
