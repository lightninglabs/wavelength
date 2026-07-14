package credit

import (
	"context"
	"encoding/hex"
	"sync"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	"github.com/stretchr/testify/require"
)

// fakeExec is an in-memory actor.Exec[creditTx] for FSM tests. Read/Stage/
// Commit each just invoke the supplied function against a zero creditTx, so a
// behavior's persist (which writes through its own Store) runs inline. The
// stageErr / commitErr hooks inject a one-shot failure to exercise the
// persist-before-effect and commit-rollback recovery paths.
type fakeExec struct {
	stageErr  error
	commitErr error

	stageCalls  int
	commitCalls int
}

func (e *fakeExec) Read(ctx context.Context,
	fn func(context.Context, creditTx) error) error {

	return fn(ctx, creditTx{})
}

func (e *fakeExec) Stage(ctx context.Context,
	fn func(context.Context, creditTx) error) error {

	e.stageCalls++
	if e.stageErr != nil {
		err := e.stageErr
		e.stageErr = nil

		return err
	}

	return fn(ctx, creditTx{})
}

func (e *fakeExec) Commit(ctx context.Context,
	fn func(context.Context, creditTx) error) error {

	e.commitCalls++
	if e.commitErr != nil {
		err := e.commitErr
		e.commitErr = nil

		return err
	}

	return fn(ctx, creditTx{})
}

// Compile-time check that fakeExec satisfies the Exec handle.
var _ actor.Exec[creditTx] = (*fakeExec)(nil)

// fakeStore is an in-memory credit.Store for FSM tests.
type fakeStore struct {
	mu     sync.Mutex
	ops    map[string]db.CreditOperationRecord
	getErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{ops: make(map[string]db.CreditOperationRecord)}
}

func (s *fakeStore) GetOperation(_ context.Context, opID string) (
	*db.CreditOperationRecord, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.getErr != nil {
		return nil, s.getErr
	}

	rec, ok := s.ops[opID]
	if !ok {
		return nil, db.ErrCreditOperationNotFound
	}
	out := rec

	return &out, nil
}

func (s *fakeStore) UpsertOperation(_ context.Context,
	rec db.CreditOperationRecord) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	s.ops[rec.OpID] = rec

	return nil
}

func (s *fakeStore) LookupActiveOperationByKey(_ context.Context, key string) (
	*db.CreditOperationRecord, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, rec := range s.ops {
		if rec.OpKey == key && rec.Status != db.CreditOpStatusFailed {
			out := rec

			return &out, nil
		}
	}

	return nil, db.ErrCreditOperationNotFound
}

func (s *fakeStore) ListNonTerminal(_ context.Context) (
	[]db.CreditOperationRecord, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	var out []db.CreditOperationRecord
	for _, rec := range s.ops {
		if !rec.Status.IsTerminal() {
			out = append(out, rec)
		}
	}

	return out, nil
}

func (s *fakeStore) ListOperations(_ context.Context) (
	[]db.CreditOperationRecord, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]db.CreditOperationRecord, 0, len(s.ops))
	for _, rec := range s.ops {
		out = append(out, rec)
	}

	return out, nil
}

// fakeServerOp is one tracked server credit operation.
type fakeServerOp struct {
	id          string
	state       ServerCreditState
	paymentHash []byte
}

// fakeServer is an in-memory CreditServer for FSM tests.
type fakeServer struct {
	mu sync.Mutex

	ops         map[string]*fakeServerOp // keyed by idempotency key
	available   uint64
	createCalls map[string]int
	redeemCalls map[string]int
	startPayCnt int
	startPayErr error
	createState ServerCreditState
	redeemState ServerCreditState
	receiveHash []byte
}

func newFakeServer() *fakeServer {
	return &fakeServer{
		ops:         make(map[string]*fakeServerOp),
		createCalls: make(map[string]int),
		redeemCalls: make(map[string]int),
		createState: ServerStateAwaitingPayment,
		redeemState: ServerStateReserved,
		receiveHash: []byte("receivehash00000000000000000000000"),
	}
}

func (s *fakeServer) CreateCredit(_ context.Context, _ []byte,
	idempotencyKey string, source CreditSource, _ uint64, _ string) (
	*CreateCreditResult, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	s.createCalls[idempotencyKey]++

	op, ok := s.ops[idempotencyKey]
	if !ok {
		op = &fakeServerOp{
			id:    "srv-" + idempotencyKey,
			state: s.createState,
		}
		s.ops[idempotencyKey] = op
	}

	res := &CreateCreditResult{
		OperationID: op.id,
		State:       op.state,
	}
	switch source {
	case SourceArkTopUp:
		res.DestinationPubkey = []byte("dest-" + idempotencyKey)

	case SourceLightningReceive:
		res.Invoice = "lnbc-" + idempotencyKey
		res.PaymentHash = s.receiveHash
	}

	return res, nil
}

func (s *fakeServer) ListCredits(_ context.Context, _ []byte) (*CreditSnapshot,
	error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	snap := &CreditSnapshot{AvailableSat: s.available}
	for _, op := range s.ops {
		snap.Operations = append(snap.Operations, ServerCreditOp{
			OperationID: op.id,
			State:       op.state,
			PaymentHash: op.paymentHash,
		})
	}

	return snap, nil
}

func (s *fakeServer) RedeemCredit(_ context.Context, _ []byte,
	idempotencyKey string, _ uint64, _ []byte) (*RedeemResult, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	s.redeemCalls[idempotencyKey]++

	op, ok := s.ops[idempotencyKey]
	if !ok {
		op = &fakeServerOp{
			id:    "srv-" + idempotencyKey,
			state: s.redeemState,
		}
		s.ops[idempotencyKey] = op
	}

	return &RedeemResult{OperationID: op.id, State: op.state}, nil
}

func (s *fakeServer) StartPay(_ context.Context, _ string, _, _ uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.startPayCnt++

	return s.startPayErr
}

// markCredited sets the tracked server op for a key to CREDITED and exposes the
// given available balance.
func (s *fakeServer) markCredited(key string, available uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if op, ok := s.ops[key]; ok {
		op.state = ServerStateCredited
	}
	s.available = available
}

// setOpState forces the tracked server op for a key into a given state.
func (s *fakeServer) setOpState(key string, state ServerCreditState) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if op, ok := s.ops[key]; ok {
		op.state = state
	}
}

// setPayOp registers a server pay operation bound to a payment hash in the
// given state, so a credit-only pay can reconcile against it via ListCredits.
func (s *fakeServer) setPayOp(paymentHash []byte, state ServerCreditState) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := "pay-op-" + hex.EncodeToString(paymentHash)
	s.ops[key] = &fakeServerOp{
		id:          key,
		state:       state,
		paymentHash: append([]byte(nil), paymentHash...),
	}
}

// fakeDaemon is an in-memory CreditDaemon for FSM tests.
type fakeDaemon struct {
	mu sync.Mutex

	sendOORCalls map[string]int
	allocCalls   int
	vtxoFound    bool
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{sendOORCalls: make(map[string]int)}
}

func (d *fakeDaemon) IdentityPubKey(context.Context) ([]byte, error) {
	return []byte("identity-pubkey"), nil
}

func (d *fakeDaemon) DustLimit(context.Context) (uint64, error) {
	return 354, nil
}

func (d *fakeDaemon) SendOOR(_ context.Context, _ []byte, _ uint64,
	idempotencyKey string) (string, error) {

	d.mu.Lock()
	defer d.mu.Unlock()

	d.sendOORCalls[idempotencyKey]++

	return "oor-" + idempotencyKey, nil
}

func (d *fakeDaemon) AllocateReceiveScript(context.Context, string) ([]byte,
	[]byte, error) {

	d.mu.Lock()
	defer d.mu.Unlock()

	d.allocCalls++

	return []byte("redeem-pubkey"), []byte("redeem-pkscript"), nil
}

func (d *fakeDaemon) FindLiveVTXOByPkScript(context.Context, []byte) (bool,
	int64, error) {

	d.mu.Lock()
	defer d.mu.Unlock()

	return d.vtxoFound, 1000, nil
}

// testBehavior builds an opBehavior wired to the supplied fakes.
func testBehavior(opID string, store Store, server CreditServer,
	daemon CreditDaemon) *opBehavior {

	return &opBehavior{
		cfg: OpActorConfig{
			OpID:         opID,
			Store:        store,
			Server:       server,
			Daemon:       daemon,
			PollInterval: DefaultPollInterval,
		},
		log: btclog.Disabled,
	}
}

// admit mimics the supervisor: it builds the durable control-plane row from an
// admission message, persists it, and restores the child from it. The child is
// then driven purely by Resume, exactly as in production -- the supervisor
// pre-writes the row and the child re-drives from it.
func admit(t *testing.T, b *opBehavior, store Store, msg CreditMsg) {
	t.Helper()

	ctx := context.Background()
	rec, snap := buildRecord(b.cfg.OpID, msg)
	require.NotNil(t, rec)

	raw, err := snap.encode()
	require.NoError(t, err)
	rec.SnapshotData = raw
	rec.SnapshotVersion = snapshotVersion

	require.NoError(t, store.UpsertOperation(ctx, *rec))
	require.NoError(t, b.restore(ctx))
}

// drive runs one turn: dispatch (which Stage-checkpoints via the Exec handle)
// then a final persist, mirroring the Receive + commitAck path without the
// actor mailbox.
func drive(t *testing.T, b *opBehavior, msg CreditDurableMsg) {
	t.Helper()

	ctx := context.Background()
	require.NoError(t, b.dispatch(ctx, msg, &fakeExec{}))
	if b.rec != nil {
		require.NoError(t, b.persist(ctx))
	}
}

// driveTurn runs one full actor turn through Receive with the supplied Exec
// handle, returning the turn error (nil on a committed turn). Unlike drive, it
// exercises the real Receive path: the commitFailed reload, the Stage
// checkpoints, and the final commitAck all run, so a fakeExec can inject a
// Stage or Commit failure and the recovery path is faithful to production.
func driveTurn(b *opBehavior, ax actor.Exec[creditTx]) error {
	res := b.Receive(
		context.Background(), &ResumeCreditOpRequest{
			OpID: b.cfg.OpID,
		},
		ax,
	)
	_, err := res.Unpack()

	return err
}

func payHash() [32]byte {
	var h [32]byte
	copy(h[:], "payment-hash-for-testing-000000000")

	return h
}

// errInjected is a sentinel transient failure used to exercise crash-recovery
// paths.
var errInjected = errInjectedT{}

type errInjectedT struct{}

func (errInjectedT) Error() string { return "injected failure" }

// TestDispatchFailsCorruptState asserts that an operation whose persisted state
// string does not decode to a known FSM state is driven to a durable terminal
// failure, so it terminal-commits and is reaped rather than being restored as a
// non-terminal row on every boot.
func TestDispatchFailsCorruptState(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	server, daemon := newFakeServer(), newFakeDaemon()
	b := testBehavior("op-corrupt", store, server, daemon)

	ctx := context.Background()
	raw, err := (&opSnapshot{}).encode()
	require.NoError(t, err)
	require.NoError(
		t,
		store.UpsertOperation(
			ctx, db.CreditOperationRecord{
				OpID:            "op-corrupt",
				OpKey:           "pay:corrupt",
				Kind:            KindPay,
				State:           "not_a_real_state",
				Status:          db.CreditOpStatusPending,
				SnapshotData:    raw,
				SnapshotVersion: snapshotVersion,
			},
		),
	)
	require.NoError(t, b.restore(ctx))
	require.True(t, b.corruptState)

	// One turn drives the corrupt row to a durable failure and commits it.
	require.NoError(t, driveTurn(b, &fakeExec{}))
	require.True(t, b.terminalCommitted)
	require.Equal(t, string(StateFailed), b.rec.State)
	require.NotEmpty(t, b.rec.LastError)

	got, gerr := store.GetOperation(ctx, "op-corrupt")
	require.NoError(t, gerr)
	require.Equal(t, string(StateFailed), got.State)
	require.Equal(t, db.CreditOpStatusFailed, got.Status)
}

// TestPayTopupCommitRollbackPreservesCheckpoint asserts the persist-before-
// effect guarantee: when the final commit rolls back after the FSM advanced and
// fired its effects, the Stage checkpoint taken after CreateCredit survives, so
// the reload-and-redrive on the next turn does NOT create a second server
// credit operation. (SendOOR, being past the checkpoint, may re-run, but is
// idempotent by op key.)
func TestPayTopupCommitRollbackPreservesCheckpoint(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	server, daemon := newFakeServer(), newFakeDaemon()
	b := testBehavior("op1", store, server, daemon)

	admit(t, b, store, &StartCreditPayRequest{
		OpKey:        "pay:abc",
		Invoice:      "lnbc1",
		PaymentHash:  payHash(),
		AmountSat:    500,
		TopupSat:     200,
		MaxCreditSat: 500,
	})

	// First turn drives through CreateCredit (checkpointed) and SendOOR,
	// but the final commit rolls back.
	err := driveTurn(b, &fakeExec{commitErr: errInjected})
	require.ErrorIs(t, err, errInjected)
	require.True(t, b.commitFailed)

	// The Stage checkpoint after CreateCredit is durable, so the row sits
	// at topup_funding even though the commit failed.
	rec, gerr := store.GetOperation(context.Background(), "op1")
	require.NoError(t, gerr)
	require.Equal(t, string(StateTopupFunding), rec.State)
	require.Equal(t, 1, server.createCalls["pay:abc"])

	// Recovery turn: reload from the checkpoint and re-drive. CreateCredit
	// is NOT re-issued — the checkpoint advanced past it — proving the
	// persist-before-effect invariant.
	server.markCredited("pay:abc", 500)
	require.NoError(t, driveTurn(b, &fakeExec{}))
	require.False(t, b.commitFailed)
	require.Equal(t, 1, server.createCalls["pay:abc"])
}

// TestPayTopupStageFailureFallsBackToIdempotency asserts the second line of
// defense: if the Stage checkpoint itself fails, the next turn reloads from the
// last durable state and re-runs CreateCredit, which is safe because it is
// idempotent by op key (the same server operation is returned).
func TestPayTopupStageFailureFallsBackToIdempotency(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	server, daemon := newFakeServer(), newFakeDaemon()
	b := testBehavior("op1", store, server, daemon)

	admit(t, b, store, &StartCreditPayRequest{
		OpKey:        "pay:abc",
		Invoice:      "lnbc1",
		PaymentHash:  payHash(),
		AmountSat:    500,
		TopupSat:     200,
		MaxCreditSat: 500,
	})

	// The checkpoint after CreateCredit fails, so nothing is persisted and
	// SendOOR is never reached.
	err := driveTurn(b, &fakeExec{stageErr: errInjected})
	require.ErrorIs(t, err, errInjected)
	require.True(t, b.commitFailed)
	require.Equal(t, 1, server.createCalls["pay:abc"])
	require.Empty(t, daemon.sendOORCalls)

	rec, gerr := store.GetOperation(context.Background(), "op1")
	require.NoError(t, gerr)
	require.Equal(t, string(StateQuoting), rec.State)

	// Recovery turn: reload from quoting and re-drive. CreateCredit re-runs
	// (the checkpoint was lost) but resolves to the same server operation,
	// and the op reaches awaiting on a single OOR transfer.
	require.NoError(t, driveTurn(b, &fakeExec{}))
	require.Equal(t, string(StateTopupAwaitingCredit), b.rec.State)
	require.Equal(t, "srv-pay:abc", b.rec.ServerOpID)
	require.Equal(t, 1, daemon.sendOORCalls["pay:abc"])
}

// TestCreditPaySettlesOnDebit asserts a credit-only pay does not complete on
// StartPay acceptance, but waits for the server ledger to report the pay
// debited.
func TestCreditPaySettlesOnDebit(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	server, daemon := newFakeServer(), newFakeDaemon()
	b := testBehavior("op1", store, server, daemon)

	admit(t, b, store, &StartCreditPayRequest{
		OpKey:        "pay:abc",
		Invoice:      "lnbc1",
		PaymentHash:  payHash(),
		AmountSat:    500,
		MaxCreditSat: 500,
		CreditOnly:   true,
	})

	// StartPay is accepted, but the pay only awaits settlement — it must
	// not report completed yet.
	drive(t, b, &ResumeCreditOpRequest{OpID: "op1"})
	require.Equal(t, string(StatePayAwaitingSettlement), b.rec.State)
	require.Equal(t, 1, server.startPayCnt)

	// The server debits the pay; the next reconcile completes it.
	ph := payHash()
	server.setPayOp(ph[:], ServerStateDebited)
	drive(t, b, &ResumeCreditOpRequest{OpID: "op1"})
	require.Equal(t, string(StateCompleted), b.rec.State)
}

// TestCreditPayFailsOnRelease asserts a credit-only pay terminal-fails when the
// server releases the reservation (the Lightning leg did not settle), rather
// than being reported as a successful send.
func TestCreditPayFailsOnRelease(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	server, daemon := newFakeServer(), newFakeDaemon()
	b := testBehavior("op1", store, server, daemon)

	admit(t, b, store, &StartCreditPayRequest{
		OpKey:        "pay:abc",
		Invoice:      "lnbc1",
		PaymentHash:  payHash(),
		AmountSat:    500,
		MaxCreditSat: 500,
		CreditOnly:   true,
	})
	drive(t, b, &ResumeCreditOpRequest{OpID: "op1"})
	require.Equal(t, string(StatePayAwaitingSettlement), b.rec.State)

	ph := payHash()
	server.setPayOp(ph[:], ServerStateReleased)
	drive(t, b, &ResumeCreditOpRequest{OpID: "op1"})
	require.Equal(t, string(StateFailed), b.rec.State)
	require.NotEmpty(t, b.rec.LastError)
}

// TestAwaitingPollCapFailsOp asserts the configurable poll cap terminal-fails
// an operation that the server never resolves, instead of parking forever.
func TestAwaitingPollCapFailsOp(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	server, daemon := newFakeServer(), newFakeDaemon()
	b := testBehavior("op1", store, server, daemon)
	b.cfg.MaxAwaitingPolls = 2

	admit(t, b, store, &StartCreditReceiveRequest{
		OpKey:     "recv:abc",
		AmountSat: 42,
	})

	// First drive creates the invoice and parks awaiting settlement.
	drive(t, b, &ResumeCreditOpRequest{OpID: "op1"})
	require.Equal(t, string(StateAwaitingSettlement), b.rec.State)

	// The server never credits; after the poll cap the op fails rather than
	// parking forever.
	drive(t, b, &ResumeCreditOpRequest{OpID: "op1"})
	require.Equal(t, string(StateAwaitingSettlement), b.rec.State)
	drive(t, b, &ResumeCreditOpRequest{OpID: "op1"})
	require.Equal(t, string(StateFailed), b.rec.State)
	require.NotEmpty(t, b.rec.LastError)
}

// TestPayNoTopupCompletes asserts a pay with no shortfall jumps straight to
// StartPay and completes in one turn.
func TestPayNoTopupCompletes(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	server, daemon := newFakeServer(), newFakeDaemon()
	b := testBehavior("op1", store, server, daemon)

	admit(t, b, store, &StartCreditPayRequest{
		OpKey:        "pay:abc",
		Invoice:      "lnbc1",
		PaymentHash:  payHash(),
		AmountSat:    500,
		TopupSat:     0,
		MaxCreditSat: 500,
	})
	drive(t, b, &ResumeCreditOpRequest{OpID: "op1"})

	require.Equal(t, string(StateCompleted), b.rec.State)
	require.Equal(t, 1, server.startPayCnt)
	require.Empty(t, daemon.sendOORCalls)
	require.Empty(t, server.createCalls)
}

// TestPayWithTopupCompletes asserts a pay with a shortfall creates a top-up,
// funds it via OOR, waits for the credit, then pays.
func TestPayWithTopupCompletes(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	server, daemon := newFakeServer(), newFakeDaemon()
	b := testBehavior("op1", store, server, daemon)

	admit(t, b, store, &StartCreditPayRequest{
		OpKey:        "pay:abc",
		Invoice:      "lnbc1",
		PaymentHash:  payHash(),
		AmountSat:    500,
		TopupSat:     200,
		MaxCreditSat: 500,
	})
	drive(t, b, &ResumeCreditOpRequest{OpID: "op1"})

	// Parked awaiting the credit.
	require.Equal(t, string(StateTopupAwaitingCredit), b.rec.State)
	require.Equal(t, 1, server.createCalls["pay:abc"])
	require.Equal(t, 1, daemon.sendOORCalls["pay:abc"])
	require.Equal(t, "oor-pay:abc", b.rec.OORSessionID)
	require.Equal(t, 0, server.startPayCnt)

	// Server credits the top-up; a resume drives to completion.
	server.markCredited("pay:abc", 500)
	drive(t, b, &ResumeCreditOpRequest{OpID: "op1"})

	require.Equal(t, string(StateCompleted), b.rec.State)
	require.Equal(t, 1, server.startPayCnt)
}

// TestPayTopupResumeIdempotent asserts repeated resumes before the credit
// finalizes never produce a second OOR transfer or a second top-up — the fix
// for the double-top-up window.
func TestPayTopupResumeIdempotent(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	server, daemon := newFakeServer(), newFakeDaemon()
	b := testBehavior("op1", store, server, daemon)

	admit(t, b, store, &StartCreditPayRequest{
		OpKey:        "pay:abc",
		Invoice:      "lnbc1",
		PaymentHash:  payHash(),
		AmountSat:    500,
		TopupSat:     200,
		MaxCreditSat: 500,
	})
	drive(t, b, &ResumeCreditOpRequest{OpID: "op1"})

	// Resume several times while the credit is still pending, including via
	// a fresh behavior that reloads from the store (simulating a restart).
	for i := 0; i < 3; i++ {
		reloaded := testBehavior("op1", store, server, daemon)
		require.NoError(t, reloaded.restore(context.Background()))
		drive(t, reloaded, &ResumeCreditOpRequest{OpID: "op1"})
		require.Equal(
			t, string(StateTopupAwaitingCredit), reloaded.rec.State,
		)
	}

	// Exactly one OOR transfer and one CreateCredit, despite the resumes.
	require.Equal(t, 1, daemon.sendOORCalls["pay:abc"])
	require.Equal(t, 1, server.createCalls["pay:abc"])
	require.Equal(t, 0, server.startPayCnt)
}

// TestReceiveCompletes asserts a credit receive creates the server invoice and
// completes once the server reports the receive credited.
func TestReceiveCompletes(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	server, daemon := newFakeServer(), newFakeDaemon()
	b := testBehavior("op1", store, server, daemon)

	admit(t, b, store, &StartCreditReceiveRequest{
		OpKey:     "recv:abc",
		AmountSat: 42,
		Memo:      "coffee",
	})
	drive(t, b, &ResumeCreditOpRequest{OpID: "op1"})

	require.Equal(t, string(StateAwaitingSettlement), b.rec.State)
	require.Equal(t, "lnbc-recv:abc", b.rec.Invoice)
	require.NotEmpty(t, b.rec.PaymentHash)

	server.markCredited("recv:abc", 42)
	drive(t, b, &ResumeCreditOpRequest{OpID: "op1"})

	require.Equal(t, string(StateCompleted), b.rec.State)
}

// TestRedeemCompletes asserts a redemption reserves with a wallet-owned
// destination and completes once the redeemed VTXO lands.
func TestRedeemCompletes(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	server, daemon := newFakeServer(), newFakeDaemon()
	b := testBehavior("op1", store, server, daemon)

	admit(t, b, store, &RedeemRequest{OpKey: "redeem:abc", AmountSat: 1000})
	drive(t, b, &ResumeCreditOpRequest{OpID: "op1"})

	require.Equal(t, string(StateAwaitingOOR), b.rec.State)
	require.Equal(t, 1, daemon.allocCalls)
	require.Equal(t, 1, server.redeemCalls["redeem:abc"])
	require.NotEmpty(t, b.redeemPkScript)

	daemon.vtxoFound = true
	drive(t, b, &ResumeCreditOpRequest{OpID: "op1"})

	require.Equal(t, string(StateCompleted), b.rec.State)
}

// TestPayTopupTerminalFailure asserts a server top-up that ends in a terminal
// failure state fails the operation deterministically rather than wedging.
func TestPayTopupTerminalFailure(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	server, daemon := newFakeServer(), newFakeDaemon()
	b := testBehavior("op1", store, server, daemon)

	admit(t, b, store, &StartCreditPayRequest{
		OpKey:        "pay:abc",
		Invoice:      "lnbc1",
		PaymentHash:  payHash(),
		AmountSat:    500,
		TopupSat:     200,
		MaxCreditSat: 500,
	})
	drive(t, b, &ResumeCreditOpRequest{OpID: "op1"})
	require.Equal(t, string(StateTopupAwaitingCredit), b.rec.State)

	// The server funding operation expires; the op fails terminally.
	server.setOpState("pay:abc", ServerStateExpired)
	drive(t, b, &ResumeCreditOpRequest{OpID: "op1"})

	require.Equal(t, string(StateFailed), b.rec.State)
	require.Equal(t, db.CreditOpStatusFailed, b.rec.Status)
	require.NotEmpty(t, b.rec.LastError)
	require.Equal(t, 0, server.startPayCnt)
}

// TestRedeemReserveIdempotentAcrossRestart asserts a restart mid-redemption
// reloads and never double-reserves or double-allocates.
func TestRedeemReserveIdempotentAcrossRestart(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	server, daemon := newFakeServer(), newFakeDaemon()
	b := testBehavior("op1", store, server, daemon)

	admit(t, b, store, &RedeemRequest{OpKey: "redeem:abc", AmountSat: 1000})
	drive(t, b, &ResumeCreditOpRequest{OpID: "op1"})
	require.Equal(t, string(StateAwaitingOOR), b.rec.State)

	// Restart: reload and resume while the VTXO has not landed yet.
	reloaded := testBehavior("op1", store, server, daemon)
	require.NoError(t, reloaded.restore(context.Background()))
	drive(t, reloaded, &ResumeCreditOpRequest{OpID: "op1"})

	require.Equal(t, string(StateAwaitingOOR), reloaded.rec.State)
	require.Equal(t, 1, daemon.allocCalls)
	require.Equal(t, 1, server.redeemCalls["redeem:abc"])
	require.NotEmpty(t, reloaded.redeemPkScript)
}

// TestReceiveTriggersRedeemOnWatermark asserts the redeem-on-receive fold: when
// auto-redeem is enabled and a settled receive pushes the earmark-adjusted
// available balance over the watermark, the receive FSM completes and emits a
// triggerRedeem the actor signals to the registry — replacing the periodic
// background sweep with a causal, event-driven trigger.
func TestReceiveTriggersRedeemOnWatermark(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	server, daemon := newFakeServer(), newFakeDaemon()
	b := testBehavior("recv1", store, server, daemon)
	b.cfg.AutoRedeemEnabled = true
	b.cfg.MinRedeemSat = 354

	admit(t, b, store, &StartCreditReceiveRequest{
		OpKey: "recv:1", AmountSat: 500,
	})

	// First turn creates the invoice and parks awaiting settlement; no
	// redeem is triggered yet.
	drive(t, b, &ResumeCreditOpRequest{OpID: "recv1"})
	require.Equal(t, string(StateAwaitingSettlement), b.rec.State)
	require.Nil(t, b.pendingRedeem)

	// Settle the receive with an over-watermark available balance: the
	// receive completes and emits the redeem trigger.
	server.markCredited("recv:1", 1000)
	drive(t, b, &ResumeCreditOpRequest{OpID: "recv1"})

	require.Equal(t, string(StateCompleted), b.rec.State)
	require.NotNil(t, b.pendingRedeem)
	require.Equal(t, uint64(1000), b.pendingRedeem.AvailableSat)
}

// TestReceiveSkipsRedeemBelowWatermark asserts a settled receive whose
// available balance does not clear the watermark completes without triggering a
// redeem, and that a disabled policy never triggers one.
func TestReceiveSkipsRedeemBelowWatermark(t *testing.T) {
	t.Parallel()

	t.Run("below watermark", func(t *testing.T) {
		t.Parallel()

		store := newFakeStore()
		server, daemon := newFakeServer(), newFakeDaemon()
		b := testBehavior("recv1", store, server, daemon)
		b.cfg.AutoRedeemEnabled = true
		b.cfg.MinRedeemSat = 354

		admit(t, b, store, &StartCreditReceiveRequest{
			OpKey: "recv:1", AmountSat: 100,
		})
		drive(t, b, &ResumeCreditOpRequest{OpID: "recv1"})
		server.markCredited("recv:1", 100)
		drive(t, b, &ResumeCreditOpRequest{OpID: "recv1"})

		require.Equal(t, string(StateCompleted), b.rec.State)
		require.Nil(t, b.pendingRedeem)
	})

	t.Run("auto-redeem disabled", func(t *testing.T) {
		t.Parallel()

		store := newFakeStore()
		server, daemon := newFakeServer(), newFakeDaemon()
		b := testBehavior("recv1", store, server, daemon)
		// AutoRedeemEnabled defaults to false.

		admit(t, b, store, &StartCreditReceiveRequest{
			OpKey: "recv:1", AmountSat: 500,
		})
		drive(t, b, &ResumeCreditOpRequest{OpID: "recv1"})
		server.markCredited("recv:1", 1000)
		drive(t, b, &ResumeCreditOpRequest{OpID: "recv1"})

		require.Equal(t, string(StateCompleted), b.rec.State)
		require.Nil(t, b.pendingRedeem)
	})
}
