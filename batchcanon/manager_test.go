package batchcanon

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

const testTimeout = 5 * time.Second

// ---------------------------------------------------------------------------
// In-memory fake Store (the real db store is tested separately; this keeps the
// manager unit test free of a batchcanon -> db import cycle).
// ---------------------------------------------------------------------------

type fakeStore struct {
	mu        sync.Mutex
	records   map[chainhash.Hash]*Record
	consumers map[chainhash.Hash][]ConsumerEdge
	applyErr  error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		records:   make(map[chainhash.Hash]*Record),
		consumers: make(map[chainhash.Hash][]ConsumerEdge),
	}
}

// ci builds a ConsumedInput from an outpoint with a placeholder non-empty
// pkScript. The mock chainsource ignores the script's value, but it must be
// non-empty so the manager arms the spend watch rather than skipping it.
func ci(op wire.OutPoint) ConsumedInput {
	return ConsumedInput{Outpoint: op, PkScript: []byte{0x51}}
}

// completeTestRecordEvidence installs the minimal immutable watch subjects on
// direct record fixtures. Manager registration tests use the stronger wire
// transaction validation path instead.
func completeTestRecordEvidence(record *Record) {
	record.BatchTx = []byte{0x00}
	record.ConfirmationPkScript = []byte{0x51}
	record.ConsumedInputs = []ConsumedInput{
		ci(wire.OutPoint{Hash: record.BatchTxID}),
	}
}

func cloneRecord(r *Record) *Record {
	cp := *r
	cp.BatchTx = append([]byte(nil), r.BatchTx...)
	cp.ConsumedInputs = append([]ConsumedInput(nil), r.ConsumedInputs...)
	for i := range cp.ConsumedInputs {
		cp.ConsumedInputs[i].PkScript = append(
			[]byte(nil), r.ConsumedInputs[i].PkScript...,
		)
	}
	cp.DependentVTXOs = append([]wire.OutPoint(nil), r.DependentVTXOs...)
	cp.ConfirmationPkScript = append(
		[]byte(nil), r.ConfirmationPkScript...,
	)

	return &cp
}

func (s *fakeStore) RegisterBatch(_ context.Context, r *Record,
	edges []ConsumerEdge) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.records[r.BatchTxID]
	completedPlaceholder := false
	if ok && len(existing.BatchTx) == 0 &&
		existing.RegistrationStage != RegistrationComplete {

		completed := cloneRecord(r)
		completed.State = StateUnseen
		completed.RegistrationStage = RegistrationRegistering
		completed.ObservationGeneration =
			existing.ObservationGeneration + 1
		if completed.ObservationGeneration == 0 {
			completed.ObservationGeneration = 1
		}
		completed.ReadyGeneration = fn.None[uint64]()
		completed.Revision = existing.Revision + 1
		completed.ConfirmationHeight = fn.None[int32]()
		completed.ConfirmationBlock = fn.None[chainhash.Hash]()
		seen := make(map[wire.OutPoint]struct{})
		completed.DependentVTXOs = nil
		for _, dependents := range [][]wire.OutPoint{
			existing.DependentVTXOs, r.DependentVTXOs,
		} {
			for _, dependent := range dependents {
				if _, duplicate := seen[dependent]; duplicate {
					continue
				}
				seen[dependent] = struct{}{}
				completed.DependentVTXOs = append(
					completed.DependentVTXOs, dependent,
				)
			}
		}
		s.records[r.BatchTxID] = completed
		completedPlaceholder = true
	}

	if ok && !completedPlaceholder {
		if !bytes.Equal(
			existing.ConfirmationPkScript, r.ConfirmationPkScript,
		) || !bytes.Equal(existing.BatchTx, r.BatchTx) ||
			existing.BatchOutputIndex != r.BatchOutputIndex ||
			existing.CSVExpiryDelta != r.CSVExpiryDelta ||
			len(existing.ConsumedInputs) != len(r.ConsumedInputs) {

			existing.RegistrationStage = RegistrationQuarantined
			existing.ReadyGeneration = fn.None[uint64]()
			existing.Revision++

			return ErrRegistrationConflict
		}

		type inputEvidence struct {
			value    int64
			pkScript []byte
		}
		inputs := make(
			map[wire.OutPoint]inputEvidence,
			len(existing.ConsumedInputs),
		)
		for _, in := range existing.ConsumedInputs {
			inputs[in.Outpoint] = inputEvidence{
				value:    in.Value,
				pkScript: in.PkScript,
			}
		}
		for _, in := range r.ConsumedInputs {
			evidence, ok := inputs[in.Outpoint]
			if !ok || evidence.value != in.Value ||
				!bytes.Equal(evidence.pkScript, in.PkScript) {

				existing.RegistrationStage =
					RegistrationQuarantined
				existing.ReadyGeneration = fn.None[uint64]()
				existing.Revision++

				return ErrRegistrationConflict
			}
		}

		seen := make(
			map[wire.OutPoint]struct{},
			len(existing.DependentVTXOs),
		)
		for _, dep := range existing.DependentVTXOs {
			seen[dep] = struct{}{}
		}
		for _, dep := range r.DependentVTXOs {
			if _, exists := seen[dep]; exists {
				continue
			}
			existing.DependentVTXOs = append(
				existing.DependentVTXOs, dep,
			)
			seen[dep] = struct{}{}
		}
	} else if !ok {
		s.records[r.BatchTxID] = cloneRecord(r)
	}

	consumerSeen := make(
		map[wire.OutPoint]ConsumerEdge, len(s.consumers[r.BatchTxID]),
	)
	for _, edge := range s.consumers[r.BatchTxID] {
		consumerSeen[edge.ConsumedVTXO] = edge
	}
	for _, edge := range edges {
		edge.ConsumerBatch = r.BatchTxID
		if existing, exists := consumerSeen[edge.ConsumedVTXO]; exists {
			if existing.ExpectedRevision != edge.ExpectedRevision ||
				!sameLineage(
					existing.CreatorLineage,
					edge.CreatorLineage,
				) {
				return ErrRegistrationConflict
			}

			continue
		}
		s.consumers[r.BatchTxID] = append(
			s.consumers[r.BatchTxID], edge,
		)
		consumerSeen[edge.ConsumedVTXO] = edge
	}

	return nil
}

func sameLineage(a, b []chainhash.Hash) bool {
	if len(a) != len(b) {
		return false
	}
	want := make(map[chainhash.Hash]struct{}, len(a))
	for _, txid := range a {
		want[txid] = struct{}{}
	}
	for _, txid := range b {
		if _, ok := want[txid]; !ok {
			return false
		}
	}

	return true
}

func (s *fakeStore) BeginReconcile(_ context.Context, txid chainhash.Hash) (
	*Record, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.records[txid]
	if !ok {
		return nil, ErrBatchNotFound
	}

	record.RegistrationStage = RegistrationReconciling
	record.ObservationGeneration++
	if record.ObservationGeneration == 0 {
		record.ObservationGeneration = 1
	}
	record.ReadyGeneration = fn.None[uint64]()
	record.Revision++

	return cloneRecord(record), nil
}

func (s *fakeStore) MarkReady(_ context.Context, txid chainhash.Hash,
	generation uint64) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.records[txid]
	if !ok {
		return ErrBatchNotFound
	}
	if record.ObservationGeneration != generation {
		return fmt.Errorf("stale batch readiness generation %d",
			generation)
	}

	record.RegistrationStage = RegistrationComplete
	record.ReadyGeneration = fn.Some(generation)
	record.Revision++

	return nil
}

func (s *fakeStore) ApplyObservation(_ context.Context,
	snapshot *ObservationSnapshot) error {

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.applyErr != nil {
		return s.applyErr
	}

	record, ok := s.records[snapshot.BatchTxID]
	if !ok {
		return ErrBatchNotFound
	}
	if record.ObservationGeneration != snapshot.Generation ||
		record.RegistrationStage == RegistrationQuarantined {
		return fmt.Errorf("stale or quarantined batch observation")
	}
	if len(record.ConsumedInputs) != len(snapshot.Inputs) {
		return fmt.Errorf("batch observation input count changed")
	}

	observations := make(
		map[wire.OutPoint]InputObservation, len(snapshot.Inputs),
	)
	for _, input := range snapshot.Inputs {
		if _, duplicate := observations[input.Outpoint]; duplicate {
			return fmt.Errorf("batch observation duplicates "+
				"input %s", input.Outpoint)
		}
		observations[input.Outpoint] = input
	}
	for i := range record.ConsumedInputs {
		input := &record.ConsumedInputs[i]
		observation, ok := observations[input.Outpoint]
		if !ok {
			return fmt.Errorf("batch observation omits input %s",
				input.Outpoint)
		}
		input.Conflicting = observation.Conflicting
		input.ConflictFinal = observation.ConflictFinal
	}

	record.State = snapshot.State
	record.ConfirmationHeight = snapshot.ConfirmationHeight
	record.ConfirmationBlock = snapshot.ConfirmationBlock
	if snapshot.Ready {
		record.RegistrationStage = RegistrationComplete
		record.ReadyGeneration = fn.Some(snapshot.Generation)
	} else {
		record.ReadyGeneration = fn.None[uint64]()
	}
	record.Revision++

	return nil
}

func (s *fakeStore) setApplyError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyErr = err
}

func (s *fakeStore) UpsertBatch(_ context.Context, r *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[r.BatchTxID] = cloneRecord(r)

	return nil
}

func (s *fakeStore) GetBatch(_ context.Context, txid chainhash.Hash) (*Record,
	error) {

	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[txid]
	if !ok {
		return nil, ErrBatchNotFound
	}

	return cloneRecord(r), nil
}

func (s *fakeStore) ListBatchesByState(_ context.Context, state State) (
	[]*Record, error) {

	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Record
	for _, r := range s.records {
		if r.State == state {
			out = append(out, cloneRecord(r))
		}
	}

	return out, nil
}

func (s *fakeStore) UpdateBatchState(_ context.Context, txid chainhash.Hash,
	state State) error {

	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.records[txid]; ok {
		r.State = state
		r.Revision++
	}

	return nil
}

func (s *fakeStore) RecordInputConflict(_ context.Context,
	batchTxid chainhash.Hash, op wire.OutPoint, conflicting,
	conflictFinal bool) error {

	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.records[batchTxid]; ok {
		for i := range r.ConsumedInputs {
			in := &r.ConsumedInputs[i]
			if in.Outpoint == op {
				in.Conflicting = conflicting
				in.ConflictFinal = conflictFinal
			}
		}
	}

	return nil
}

func (s *fakeStore) RecordConfirmation(_ context.Context, txid chainhash.Hash,
	height int32, block chainhash.Hash) error {

	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.records[txid]; ok {
		r.ConfirmationHeight = fn.Some(height)
		r.ConfirmationBlock = fn.Some(block)
	}

	return nil
}

func (s *fakeStore) ClearConfirmation(_ context.Context,
	txid chainhash.Hash) error {

	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.records[txid]; ok {
		r.ConfirmationHeight = fn.None[int32]()
		r.ConfirmationBlock = fn.None[chainhash.Hash]()
	}

	return nil
}

func (s *fakeStore) FindBatchesConsumingOutpoint(_ context.Context,
	op wire.OutPoint) ([]chainhash.Hash, error) {

	s.mu.Lock()
	defer s.mu.Unlock()
	var out []chainhash.Hash
	for txid, r := range s.records {
		for _, in := range r.ConsumedInputs {
			if in.Outpoint == op {
				out = append(out, txid)
			}
		}
	}

	return out, nil
}

func (s *fakeStore) ListPendingConsumerEdges(_ context.Context,
	consumerBatch chainhash.Hash) ([]ConsumerEdge, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]ConsumerEdge(nil), s.consumers[consumerBatch]...), nil
}

func (s *fakeStore) ListPendingConsumerBatchesByCreator(_ context.Context,
	creatorBatch chainhash.Hash) ([]chainhash.Hash, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	seen := make(map[chainhash.Hash]struct{})
	consumers := make([]chainhash.Hash, 0)
	for consumer, edges := range s.consumers {
		for _, edge := range edges {
			for _, creator := range edge.CreatorLineage {
				if creator != creatorBatch {
					continue
				}
				if _, ok := seen[consumer]; !ok {
					seen[consumer] = struct{}{}
					consumers = append(consumers, consumer)
				}
			}
		}
	}

	return consumers, nil
}

func (s *fakeStore) ResolveConsumerEdge(_ context.Context, edge ConsumerEdge,
	restore bool) (ConsumerEdgeResolution, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	edges := s.consumers[edge.ConsumerBatch]
	for i, pending := range edges {
		if pending.ConsumedVTXO != edge.ConsumedVTXO ||
			pending.ExpectedRevision != edge.ExpectedRevision {

			continue
		}
		s.consumers[edge.ConsumerBatch] = append(
			edges[:i], edges[i+1:]...,
		)
		if restore {
			return ConsumerEdgeRestored, nil
		}

		return ConsumerEdgeCompleted, nil
	}

	return ConsumerEdgeDeferred, nil
}

func (s *fakeStore) DeleteProvisionalConsumersForBatch(_ context.Context,
	consumerBatch chainhash.Hash) error {

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.consumers, consumerBatch)

	return nil
}

var _ Store = (*fakeStore)(nil)

// ---------------------------------------------------------------------------
// Mock chainsource actor: captures the reorg-aware notification refs from
// register requests and lets the test fire lifecycle events back at them.
// ---------------------------------------------------------------------------

type confRefs struct {
	confirmed actor.TellOnlyRef[chainsource.ConfirmationEvent]
	reorged   actor.TellOnlyRef[chainsource.ConfReorgedEvent]
	done      actor.TellOnlyRef[chainsource.ConfDoneEvent]
}

type spendRefs struct {
	spend   actor.TellOnlyRef[chainsource.SpendEvent]
	reorged actor.TellOnlyRef[chainsource.SpendReorgedEvent]
	done    actor.TellOnlyRef[chainsource.SpendDoneEvent]
}

type mockChainSource struct {
	mu          sync.Mutex
	bestHeight  int32
	confByTxid  map[chainhash.Hash]confRefs
	spendByOp   map[wire.OutPoint]map[string]spendRefs
	confCancels map[chainhash.Hash]int
	spendCancel map[wire.OutPoint]int
}

func newMockChainSource(bestHeight int32) *mockChainSource {
	return &mockChainSource{
		bestHeight:  bestHeight,
		confByTxid:  make(map[chainhash.Hash]confRefs),
		spendByOp:   make(map[wire.OutPoint]map[string]spendRefs),
		confCancels: make(map[chainhash.Hash]int),
		spendCancel: make(map[wire.OutPoint]int),
	}
}

func (c *mockChainSource) Receive(_ context.Context,
	msg chainsource.ChainSourceMsg) fn.Result[chainsource.ChainSourceResp] {

	switch v := msg.(type) {
	case *chainsource.BestHeightRequest:
		c.mu.Lock()
		h := c.bestHeight
		c.mu.Unlock()

		return fn.Ok[chainsource.ChainSourceResp](
			&chainsource.BestHeightResponse{
				Height: h,
			},
		)

	case *chainsource.RegisterConfRequest:
		c.mu.Lock()
		c.confByTxid[*v.Txid] = confRefs{
			confirmed: v.NotifyActor.UnwrapOr(nil),
			reorged:   v.NotifyReorged.UnwrapOr(nil),
			done:      v.NotifyDone.UnwrapOr(nil),
		}
		c.mu.Unlock()

		return fn.Ok[chainsource.ChainSourceResp](
			&chainsource.RegisterConfResponse{},
		)

	case *chainsource.RegisterSpendRequest:
		c.mu.Lock()
		registrations, ok := c.spendByOp[*v.Outpoint]
		if !ok {
			registrations = make(map[string]spendRefs)
			c.spendByOp[*v.Outpoint] = registrations
		}
		registrations[v.CallerID] = spendRefs{
			spend:   v.NotifyActor.UnwrapOr(nil),
			reorged: v.NotifyReorged.UnwrapOr(nil),
			done:    v.NotifyDone.UnwrapOr(nil),
		}
		c.mu.Unlock()

		return fn.Ok[chainsource.ChainSourceResp](
			&chainsource.RegisterSpendResponse{},
		)

	case *chainsource.UnregisterConfRequest:
		c.mu.Lock()
		c.confCancels[*v.Txid]++
		c.mu.Unlock()

		return fn.Ok[chainsource.ChainSourceResp](
			&chainsource.UnregisterConfResponse{},
		)

	case *chainsource.UnregisterSpendRequest:
		c.mu.Lock()
		c.spendCancel[*v.Outpoint]++
		c.mu.Unlock()

		return fn.Ok[chainsource.ChainSourceResp](
			&chainsource.UnregisterSpendResponse{},
		)

	default:
		return fn.Err[chainsource.ChainSourceResp](
			errUnexpected(msg),
		)
	}
}

func errUnexpected(msg chainsource.ChainSourceMsg) error {
	return &unexpectedMsgErr{msg: msg.MessageType()}
}

type unexpectedMsgErr struct{ msg string }

func (e *unexpectedMsgErr) Error() string {
	return "mock chainsource: unexpected message " + e.msg
}

// getConfRefs waits until the manager has registered a conf watch for txid and
// returns the captured refs.
func (c *mockChainSource) getConfRefs(t *testing.T,
	txid chainhash.Hash) confRefs {

	t.Helper()
	var refs confRefs
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		r, ok := c.confByTxid[txid]
		if ok {
			refs = r
		}

		return ok
	}, testTimeout, 5*time.Millisecond, "conf watch never registered")

	return refs
}

func (c *mockChainSource) getSpendRefs(t *testing.T,
	op wire.OutPoint) []spendRefs {

	t.Helper()
	var refs []spendRefs
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		registrations, ok := c.spendByOp[op]
		if ok && len(registrations) > 0 {
			refs = refs[:0]
			for _, registration := range registrations {
				refs = append(refs, registration)
			}
		}

		return len(refs) > 0
	}, testTimeout, 5*time.Millisecond, "spend watch never registered")

	return refs
}

func (c *mockChainSource) spendCancelCount(op wire.OutPoint) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.spendCancel[op]
}

// ---------------------------------------------------------------------------
// Harness.
// ---------------------------------------------------------------------------

type managerHarness struct {
	mgrRef actor.ActorRef[ManagerMsg, ManagerResp]
	mgr    *Manager
	mock   *mockChainSource
	store  *fakeStore
}

func newManagerHarness(t *testing.T, bestHeight int32) *managerHarness {
	return newManagerHarnessWithRestore(t, bestHeight, nil)
}

// newManagerHarnessWithRestore is newManagerHarness with a RestoreConsumedVTXO
// callback wired into the manager config, for the reverse-dependency
// (provisional-forfeit restore) tests.
func newManagerHarnessWithRestore(t *testing.T, bestHeight int32,
	restore func(ctx context.Context, vtxo wire.OutPoint) error,
) *managerHarness {

	t.Helper()

	mock := newMockChainSource(bestHeight)
	mockActor := actor.NewActor(actor.ActorConfig[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]{
		ID:          "mock-chainsource",
		Behavior:    mock,
		MailboxSize: 64,
	})
	mockActor.Start()
	t.Cleanup(mockActor.Stop)

	store := newFakeStore()
	mgr := NewManager(ManagerConfig{
		Store:                       store,
		ChainSource:                 mockActor.Ref(),
		ActivateRestoredVTXO:        restore,
		allowIncompleteTestEvidence: true,
	})
	mgrActor := actor.NewActor(actor.ActorConfig[ManagerMsg, ManagerResp]{
		ID:          "batch-canonicality",
		Behavior:    mgr,
		MailboxSize: 64,
	})
	mgr.SetSelfRef(mgrActor.TellRef())
	mgrActor.Start()
	t.Cleanup(mgrActor.Stop)

	return &managerHarness{
		mgrRef: mgrActor.Ref(),
		mgr:    mgr,
		mock:   mock,
		store:  store,
	}
}

// registerBatch registers a batch and waits for the synchronous response.
func (h *managerHarness) registerBatch(t *testing.T,
	req *RegisterBatchRequest) {

	t.Helper()
	if len(req.ConfirmationPkScript) == 0 {
		req.ConfirmationPkScript = []byte{0x51}
	}
	if len(req.ConsumedInputs) == 0 {
		req.ConsumedInputs = []ConsumedInput{ci(wire.OutPoint{
			Hash: req.BatchTxID,
		})}
	}
	_, err := h.mgrRef.Ask(t.Context(), req).Await(t.Context()).Unpack()
	require.NoError(t, err)
}

// state reads the persisted record for a batch via the manager. Because the
// manager mailbox is FIFO, issuing this Ask after a fired event guarantees the
// event was processed first.
func (h *managerHarness) state(t *testing.T,
	txid chainhash.Hash) *GetBatchStateResponse {

	t.Helper()
	resp, err := h.mgrRef.Ask(
		t.Context(), &GetBatchStateRequest{BatchTxID: txid},
	).Await(t.Context()).Unpack()
	require.NoError(t, err)
	got, ok := resp.(*GetBatchStateResponse)
	require.True(t, ok)

	return got
}

// fire helpers Tell the captured chainsource refs, synchronously enqueuing the
// re-wrapped event onto the manager mailbox.
func (h *managerHarness) fireConfirmed(t *testing.T, txid chainhash.Hash,
	height int32, block chainhash.Hash) {

	t.Helper()
	refs := h.mock.getConfRefs(t, txid)
	require.NoError(
		t,
		refs.confirmed.Tell(
			t.Context(), chainsource.ConfirmationEvent{
				Txid:        txid,
				BlockHeight: height,
				BlockHash:   block,
				NumConfs:    1,
			},
		),
	)
}

func (h *managerHarness) fireConfReorged(t *testing.T, txid chainhash.Hash) {
	t.Helper()
	refs := h.mock.getConfRefs(t, txid)
	require.NoError(
		t,
		refs.reorged.Tell(
			t.Context(), chainsource.ConfReorgedEvent{
				Txid: txid,
			},
		),
	)
}

func (h *managerHarness) fireConfDone(t *testing.T, txid chainhash.Hash) {
	t.Helper()
	refs := h.mock.getConfRefs(t, txid)
	require.NoError(
		t,
		refs.done.Tell(
			t.Context(), chainsource.ConfDoneEvent{
				Txid: txid,
			},
		),
	)
}

func (h *managerHarness) fireSpend(t *testing.T, op wire.OutPoint,
	spender chainhash.Hash, height int32) {

	t.Helper()
	for _, refs := range h.mock.getSpendRefs(t, op) {
		require.NoError(
			t,
			refs.spend.Tell(
				t.Context(), chainsource.SpendEvent{
					Outpoint:       op,
					SpendingTxid:   spender,
					SpendingHeight: height,
				},
			),
		)
	}
}

func (h *managerHarness) fireSpendReorged(t *testing.T, op wire.OutPoint) {
	t.Helper()
	for _, refs := range h.mock.getSpendRefs(t, op) {
		require.NoError(
			t,
			refs.reorged.Tell(
				t.Context(), chainsource.SpendReorgedEvent{
					Outpoint: op,
				},
			),
		)
	}
}

func (h *managerHarness) fireSpendDone(t *testing.T, op wire.OutPoint) {
	t.Helper()
	for _, refs := range h.mock.getSpendRefs(t, op) {
		require.NoError(
			t,
			refs.done.Tell(
				t.Context(), chainsource.SpendDoneEvent{
					Outpoint: op,
				},
			),
		)
	}
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

func testBatchTxid(b byte) chainhash.Hash {
	var h chainhash.Hash
	h[0] = b

	return h
}

func testOutpoint(b byte, idx uint32) wire.OutPoint {
	return wire.OutPoint{Hash: chainhash.Hash{b}, Index: idx}
}

// TestManagerConfirmThenFinalize drives the happy path: a registered batch is
// unseen, becomes provisional on first confirmation (with a derived effective
// expiry), then finalized on the chainsource Done.
func TestManagerConfirmThenFinalize(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	txid := testBatchTxid(0xaa)

	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            txid,
		ConfirmationPkScript: []byte{0x51, 0x20, 0x01},
		CSVExpiryDelta:       144,
	})

	// Unseen before any observation.
	got := h.state(t, txid)
	require.True(t, got.Found)
	require.Equal(t, StateUnseen, got.Record.State)
	require.True(t, got.Record.EffectiveExpiry().IsNone())

	// First confirmation -> provisional, effective expiry derived.
	h.fireConfirmed(t, txid, 101, testBatchTxid(0xb1))
	got = h.state(t, txid)
	require.Equal(t, StateProvisional, got.Record.State)
	require.Equal(t, int32(101), got.Record.ConfirmationHeight.UnwrapOr(0))
	require.Equal(t, int32(245), got.Record.EffectiveExpiry().UnwrapOr(0))

	// Policy finality -> finalized.
	h.fireConfDone(t, txid)
	got = h.state(t, txid)
	require.Equal(t, StateFinalized, got.Record.State)
}

// TestManagerReorgRecovers proves the core reorg-safety property: a confirmed
// batch that is reorged out moves to reorged_out (with expiry erased), then
// recovers to provisional on reconfirmation at a new height (with a fresh
// effective expiry), then finalizes.
func TestManagerReorgRecovers(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	txid := testBatchTxid(0xcc)

	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:      txid,
		CSVExpiryDelta: 100,
	})

	h.fireConfirmed(t, txid, 101, testBatchTxid(0xd1))
	got := h.state(t, txid)
	require.Equal(t, StateProvisional, got.Record.State)
	require.Equal(t, int32(201), got.Record.EffectiveExpiry().UnwrapOr(0))

	// Reorg out: state reorged_out, confirmation (and effective expiry)
	// cleared.
	h.fireConfReorged(t, txid)
	got = h.state(t, txid)
	require.Equal(t, StateReorgedOut, got.Record.State)
	require.True(t, got.Record.ConfirmationHeight.IsNone())
	require.True(t, got.Record.EffectiveExpiry().IsNone())

	// Reconfirm at a higher height: provisional again, fresh expiry.
	h.fireConfirmed(t, txid, 105, testBatchTxid(0xd2))
	got = h.state(t, txid)
	require.Equal(t, StateProvisional, got.Record.State)
	require.Equal(t, int32(205), got.Record.EffectiveExpiry().UnwrapOr(0))

	h.fireConfDone(t, txid)
	require.Equal(t, StateFinalized, h.state(t, txid).Record.State)
}

// TestManagerInputConflict proves conflict detection: a consumed input spent
// by a transaction OTHER than the batch is a conflict (conflict_provisional),
// promoted to conflict_finalized once the conflicting spend matures.
func TestManagerInputConflict(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	txid := testBatchTxid(0x11)
	input := testOutpoint(0x22, 0)

	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:      txid,
		CSVExpiryDelta: 50,
		ConsumedInputs: []ConsumedInput{ci(input)},
	})

	h.fireConfirmed(t, txid, 101, testBatchTxid(0x33))
	require.Equal(t, StateProvisional, h.state(t, txid).Record.State)

	// A different tx double-spends the consumed input.
	conflictTx := testBatchTxid(0x99)
	h.fireSpend(t, input, conflictTx, 102)
	require.Equal(
		t, StateConflictProvisional, h.state(t, txid).Record.State,
	)

	// The conflict matures -> conflict_finalized.
	h.fireSpendDone(t, input)
	require.Equal(
		t, StateConflictFinalized, h.state(t, txid).Record.State,
	)
}

// TestManagerConflictClearsOnSpendReorg proves a conflict is reversible: if the
// conflicting spend is itself reorged out, the batch returns to its
// confirmation-derived state.
func TestManagerConflictClearsOnSpendReorg(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	txid := testBatchTxid(0x41)
	input := testOutpoint(0x42, 1)

	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:      txid,
		CSVExpiryDelta: 50,
		ConsumedInputs: []ConsumedInput{ci(input)},
	})
	h.fireConfirmed(t, txid, 101, testBatchTxid(0x43))

	h.fireSpend(t, input, testBatchTxid(0x99), 102)
	require.Equal(
		t, StateConflictProvisional, h.state(t, txid).Record.State,
	)

	// The conflicting spend reorgs out -> conflict cleared, back to
	// provisional (the batch is still confirmed).
	h.fireSpendReorged(t, input)
	require.Equal(t, StateProvisional, h.state(t, txid).Record.State)
}

// TestManagerBatchSelfSpendNotConflict proves that the batch consuming its own
// input (the expected case) is not treated as a conflict.
func TestManagerBatchSelfSpendNotConflict(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	txid := testBatchTxid(0x51)
	input := testOutpoint(0x52, 0)

	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:      txid,
		CSVExpiryDelta: 50,
		ConsumedInputs: []ConsumedInput{ci(input)},
	})
	h.fireConfirmed(t, txid, 101, testBatchTxid(0x53))

	// The spend is by the batch itself: not a conflict.
	h.fireSpend(t, input, txid, 101)
	require.Equal(t, StateProvisional, h.state(t, txid).Record.State)

	// And its maturation is the normal consumption, not a conflict.
	h.fireSpendDone(t, input)
	require.Equal(t, StateProvisional, h.state(t, txid).Record.State)
}

// TestManagerConflictDominatesReorg proves the state priority: when a batch is
// both reorged out AND has a conflicting input spend, conflict dominates.
func TestManagerConflictDominatesReorg(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	txid := testBatchTxid(0x61)
	input := testOutpoint(0x62, 0)

	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:      txid,
		CSVExpiryDelta: 50,
		ConsumedInputs: []ConsumedInput{ci(input)},
	})
	h.fireConfirmed(t, txid, 101, testBatchTxid(0x63))
	h.fireConfReorged(t, txid)
	require.Equal(t, StateReorgedOut, h.state(t, txid).Record.State)

	// A conflicting spend appears while the batch is reorged out: conflict
	// dominates reorged_out.
	h.fireSpend(t, input, testBatchTxid(0x99), 102)
	require.Equal(
		t, StateConflictProvisional, h.state(t, txid).Record.State,
	)
}

// TestManagerFinalizeReleasesSpendWatches proves the manager waits for every
// current-generation subject before releasing watches, even if confirmation
// Done arrives ahead of the input's own-spend observation.
func TestManagerFinalizeReleasesSpendWatches(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	txid := testBatchTxid(0x71)
	input := testOutpoint(0x72, 0)

	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:      txid,
		CSVExpiryDelta: 50,
		ConsumedInputs: []ConsumedInput{ci(input)},
	})
	h.fireConfirmed(t, txid, 101, testBatchTxid(0x73))
	h.fireConfDone(t, txid)
	require.Equal(t, 0, h.mock.spendCancelCount(input))
	h.fireSpend(t, input, txid, 101)

	// Drain via a state read, then assert the complete terminal snapshot
	// released the spend watch.
	require.Equal(t, StateFinalized, h.state(t, txid).Record.State)
	require.Eventually(t, func() bool {
		return h.mock.spendCancelCount(input) == 1
	}, testTimeout, 5*time.Millisecond,
		"input spend watch not released on finalize")
}

// TestManagerRegisterIdempotentMergesDependents proves a repeat registration
// merges dependent VTXOs without re-arming or losing state.
func TestManagerRegisterIdempotentMergesDependents(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	txid := testBatchTxid(0x81)
	depA := testOutpoint(0x8a, 0)
	depB := testOutpoint(0x8b, 0)

	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:      txid,
		CSVExpiryDelta: 50,
		DependentVTXOs: []wire.OutPoint{depA},
	})
	h.fireConfirmed(t, txid, 101, testBatchTxid(0x83))
	require.Equal(t, StateProvisional, h.state(t, txid).Record.State)

	// Repeat with an additional dependent: merged, state preserved.
	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:      txid,
		CSVExpiryDelta: 50,
		DependentVTXOs: []wire.OutPoint{depB},
	})
	got := h.state(t, txid)
	require.Equal(t, StateProvisional, got.Record.State)
	require.ElementsMatch(
		t, []wire.OutPoint{depA, depB}, got.Record.DependentVTXOs,
	)
}

// TestManagerReconcileReArmsWatches proves restart reconciliation: a manager
// started against a store with a persisted provisional batch re-arms its
// watches and does not downgrade the persisted state before re-observation.
func TestManagerReconcileReArmsWatches(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	txid := testBatchTxid(0x91)
	input := testOutpoint(0x92, 0)

	// Seed a persisted provisional batch as if a prior run had observed it.
	require.NoError(
		t,
		store.UpsertBatch(
			t.Context(), &Record{
				BatchTxID:            txid,
				BatchTx:              []byte{0x00},
				State:                StateProvisional,
				ConfirmationHeight:   fn.Some[int32](90),
				CSVExpiryDelta:       50,
				ConfirmationPkScript: []byte{0x51},
				ConsumedInputs: []ConsumedInput{
					ci(input),
				},
			},
		),
	)

	mock := newMockChainSource(100)
	mockActor := actor.NewActor(actor.ActorConfig[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]{ID: "mock", Behavior: mock, MailboxSize: 64})
	mockActor.Start()
	t.Cleanup(mockActor.Stop)

	mgr := NewManager(
		ManagerConfig{
			Store:       store,
			ChainSource: mockActor.Ref(),
		},
	)
	mgrActor := actor.NewActor(actor.ActorConfig[ManagerMsg, ManagerResp]{
		ID: "mgr", Behavior: mgr, MailboxSize: 64,
	})
	mgr.SetSelfRef(mgrActor.TellRef())
	mgrActor.Start()
	t.Cleanup(mgrActor.Stop)

	require.NoError(t, mgr.Reconcile(t.Context()))

	// Watches re-armed for the persisted batch.
	mock.getConfRefs(t, txid)
	mock.getSpendRefs(t, input)

	// State not downgraded by reconcile.
	h := &managerHarness{mgrRef: mgrActor.Ref(), mock: mock, store: store}
	require.Equal(t, StateProvisional, h.state(t, txid).Record.State)

	// A reorg after restart is still handled correctly.
	h.fireConfReorged(t, txid)
	require.Equal(t, StateReorgedOut, h.state(t, txid).Record.State)
}

// TestManagerReconcileConflictNotDowngradedByConfReplay proves that a persisted
// conflict_provisional batch is NOT transiently downgraded to provisional when,
// after a restart, the confirmation is re-observed before the conflicting spend
// is re-observed. Reconciliation seeds the per-input conflict view from the
// persisted flags, so a bare re-confirmation cannot clear a conflict it did not
// resolve. Without the fix this asserted state 4 (conflict_provisional) but got
// state 1 (provisional) — a window in which the coin would be wrongly admitted.
func TestManagerReconcileConflictNotDowngradedByConfReplay(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	txid := testBatchTxid(0x93)
	input := testOutpoint(0x94, 0)

	// Seed a persisted conflict_provisional batch whose consumed input was
	// observed conflicting by a prior run: confirmed, but a foreign tx
	// double-spent its input.
	require.NoError(
		t,
		store.UpsertBatch(
			t.Context(), &Record{
				BatchTxID:            txid,
				BatchTx:              []byte{0x00},
				State:                StateConflictProvisional,
				ConfirmationHeight:   fn.Some[int32](90),
				CSVExpiryDelta:       50,
				ConfirmationPkScript: []byte{0x51},
				ConsumedInputs: []ConsumedInput{{
					Outpoint:    input,
					PkScript:    []byte{0x51},
					Conflicting: true,
				}},
			},
		),
	)

	mock := newMockChainSource(100)
	mockActor := actor.NewActor(actor.ActorConfig[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]{ID: "mock", Behavior: mock, MailboxSize: 64})
	mockActor.Start()
	t.Cleanup(mockActor.Stop)

	mgr := NewManager(
		ManagerConfig{
			Store:       store,
			ChainSource: mockActor.Ref(),
		},
	)
	mgrActor := actor.NewActor(actor.ActorConfig[ManagerMsg, ManagerResp]{
		ID: "mgr", Behavior: mgr, MailboxSize: 64,
	})
	mgr.SetSelfRef(mgrActor.TellRef())
	mgrActor.Start()
	t.Cleanup(mgrActor.Stop)

	require.NoError(t, mgr.Reconcile(t.Context()))

	h := &managerHarness{mgrRef: mgrActor.Ref(), mock: mock, store: store}

	// Reconcile alone must preserve the persisted conflict.
	require.Equal(
		t, StateConflictProvisional, h.state(t, txid).Record.State,
	)

	// The confirmation is re-observed first (the batch is still mined).
	// This must NOT clear the conflict: the conflicting spend has not been
	// observed to reorg away.
	h.fireConfirmed(t, txid, 90, testBatchTxid(0x95))
	require.Equal(
		t, StateConflictProvisional, h.state(t, txid).Record.State,
		"re-confirmation wrongly downgraded a persisted conflict",
	)

	// The conflict clears only when the conflicting spend is observed to
	// reorg out — then, and only then, the batch returns to provisional.
	h.fireSpendReorged(t, input)
	require.Equal(t, StateProvisional, h.state(t, txid).Record.State)
}
