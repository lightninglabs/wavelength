package unroll

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/recovery"
	"github.com/lightninglabs/darepo-client/unrollplan"
	"github.com/lightninglabs/darepo-client/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// memRegistryStore is a minimal in-memory registry control-plane store.
type memRegistryStore struct {
	mu      sync.Mutex
	records map[wire.OutPoint]RegistryRecord
}

// newMemRegistryStore creates a new in-memory registry store.
func newMemRegistryStore() *memRegistryStore {
	return &memRegistryStore{
		records: make(map[wire.OutPoint]RegistryRecord),
	}
}

// UpsertRecord stores one registry record.
func (s *memRegistryStore) UpsertRecord(_ context.Context,
	record RegistryRecord) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	s.records[record.TargetOutpoint] = cloneRegistryRecord(record)

	return nil
}

// GetRecord returns one registry record when present.
func (s *memRegistryStore) GetRecord(_ context.Context, target wire.OutPoint) (
	*RegistryRecord, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.records[target]
	if !ok {
		return nil, nil
	}

	cloned := cloneRegistryRecord(record)

	return &cloned, nil
}

// ListNonTerminalRecords returns all non-terminal records.
func (s *memRegistryStore) ListNonTerminalRecords(_ context.Context) (
	[]RegistryRecord, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]RegistryRecord, 0, len(s.records))
	for _, record := range s.records {
		if record.IsTerminal() {
			continue
		}

		result = append(result, cloneRegistryRecord(record))
	}

	return result, nil
}

// MarkTerminal records one terminal phase.
func (s *memRegistryStore) MarkTerminal(_ context.Context, target wire.OutPoint,
	phase Phase, recoverable bool, failReason string,
	sweepTxid *chainhash.Hash) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	record := s.records[target]
	record.TargetOutpoint = target
	record.Phase = phase
	record.RecoverableFailure = recoverable
	record.FailReason = failReason
	record.SweepTxid = copyHash(sweepTxid)
	s.records[target] = record

	return nil
}

// flakyRegistryStore fails a configured number of initial upserts, then
// behaves like the in-memory store.
type flakyRegistryStore struct {
	*memRegistryStore

	mu           sync.Mutex
	upsertErrors int
}

// newFlakyRegistryStore creates a store that fails the first n upserts.
func newFlakyRegistryStore(upsertErrors int) *flakyRegistryStore {
	return &flakyRegistryStore{
		memRegistryStore: newMemRegistryStore(),
		upsertErrors:     upsertErrors,
	}
}

// UpsertRecord stores one registry record unless this store is still
// configured to fail.
func (s *flakyRegistryStore) UpsertRecord(ctx context.Context,
	record RegistryRecord) error {

	s.mu.Lock()
	if s.upsertErrors > 0 {
		s.upsertErrors--
		s.mu.Unlock()

		return errors.New("injected upsert failure")
	}
	s.mu.Unlock()

	return s.memRegistryStore.UpsertRecord(ctx, record)
}

// terminalFlakyRegistryStore fails a configured number of terminal-phase
// upserts, then behaves like the in-memory store. Non-terminal writes
// always succeed so the fail-closed admission path is not interfered
// with; only the terminal retry loop is exercised.
type terminalFlakyRegistryStore struct {
	*memRegistryStore

	mu           sync.Mutex
	upsertErrors int
}

// newTerminalFlakyRegistryStore creates a store that fails the first n
// terminal-phase upserts.
func newTerminalFlakyRegistryStore(
	upsertErrors int) *terminalFlakyRegistryStore {

	return &terminalFlakyRegistryStore{
		memRegistryStore: newMemRegistryStore(),
		upsertErrors:     upsertErrors,
	}
}

// UpsertRecord stores non-terminal records inline and fails the first N
// terminal upserts so the registry's terminal retry path is exercised.
func (s *terminalFlakyRegistryStore) UpsertRecord(ctx context.Context,
	record RegistryRecord) error {

	if !record.IsTerminal() {
		return s.memRegistryStore.UpsertRecord(ctx, record)
	}

	s.mu.Lock()
	if s.upsertErrors > 0 {
		s.upsertErrors--
		s.mu.Unlock()

		return errors.New("injected terminal upsert failure")
	}
	s.mu.Unlock()

	return s.memRegistryStore.UpsertRecord(ctx, record)
}

// alwaysFailUpsertRegistryStore rejects every terminal-phase upsert while
// keeping the non-terminal initial write and all read paths backed by the
// in-memory store. This matches the fail-closed admission contract that the
// registry now enforces on EnsureUnroll: the initial record must land
// durably for the admission to succeed, but subsequent terminal updates
// flow through the async retry path so tests can exercise the retry loop.
type alwaysFailUpsertRegistryStore struct {
	*memRegistryStore
}

// newAlwaysFailUpsertRegistryStore creates a store that rejects every
// terminal-phase upsert while letting the initial Pending write succeed.
func newAlwaysFailUpsertRegistryStore() *alwaysFailUpsertRegistryStore {
	return &alwaysFailUpsertRegistryStore{
		memRegistryStore: newMemRegistryStore(),
	}
}

// UpsertRecord lets non-terminal writes through and fails every terminal
// write so the registry's async retry machinery is exercised without
// preventing the fail-closed admission write from succeeding.
func (s *alwaysFailUpsertRegistryStore) UpsertRecord(ctx context.Context,
	record RegistryRecord) error {

	if !record.IsTerminal() {
		return s.memRegistryStore.UpsertRecord(ctx, record)
	}

	return errors.New("injected upsert failure")
}

// blockingRegistryStore holds terminal-phase upserts until released so tests
// can verify that registry status remains available while persistence is
// stalled. Non-terminal writes (including the fail-closed admission write
// in EnsureUnroll) pass through immediately so spawning still completes.
type blockingRegistryStore struct {
	*memRegistryStore

	started chan struct{}
	release chan struct{}
	once    sync.Once
}

// newBlockingRegistryStore creates a store with a gate around terminal-phase
// UpsertRecord calls; non-terminal writes proceed inline.
func newBlockingRegistryStore() *blockingRegistryStore {
	return &blockingRegistryStore{
		memRegistryStore: newMemRegistryStore(),
		started:          make(chan struct{}),
		release:          make(chan struct{}),
	}
}

// UpsertRecord passes non-terminal writes straight through and waits on the
// release gate for terminal writes so tests can observe the registry while
// the terminal persist is stalled.
func (s *blockingRegistryStore) UpsertRecord(ctx context.Context,
	record RegistryRecord) error {

	if !record.IsTerminal() {
		return s.memRegistryStore.UpsertRecord(ctx, record)
	}

	s.once.Do(func() {
		close(s.started)
	})

	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}

	return s.memRegistryStore.UpsertRecord(ctx, record)
}

// cancelOnPendingRegistryStore cancels the caller context after the initial
// pending admission row has been persisted.
type cancelOnPendingRegistryStore struct {
	*memRegistryStore

	cancel context.CancelFunc
}

// UpsertRecord stores the record and cancels on the first pending write.
func (s *cancelOnPendingRegistryStore) UpsertRecord(ctx context.Context,
	record RegistryRecord) error {

	err := s.memRegistryStore.UpsertRecord(ctx, record)
	if err == nil && record.Phase == PhasePending {
		s.cancel()
	}

	return err
}

// fakeRegistryChainSourceRef is a minimal chainsource actor ref for registry
// tests.
type fakeRegistryChainSourceRef struct {
	height int32
}

// ID returns the fake actor ID.
func (f *fakeRegistryChainSourceRef) ID() string {
	return "fake-registry-chain"
}

// Tell is unused by registry tests.
func (f *fakeRegistryChainSourceRef) Tell(_ context.Context,
	msg chainsource.ChainSourceMsg) error {

	switch msg.(type) {
	case *chainsource.UnsubscribeBlocksRequest:
		return nil

	case *chainsource.UnregisterSpendRequest:
		return nil
	}

	return nil
}

// Ask returns fixed best-height and fee-estimate responses.
func (f *fakeRegistryChainSourceRef) Ask(_ context.Context,
	msg chainsource.ChainSourceMsg,
) actor.Future[chainsource.ChainSourceResp] {

	promise := actor.NewPromise[chainsource.ChainSourceResp]()

	switch msg.(type) {
	case *chainsource.BestHeightRequest:
		promise.Complete(
			fn.Ok[chainsource.ChainSourceResp](
				&chainsource.BestHeightResponse{
					Height: f.height,
				},
			),
		)

	case *chainsource.FeeEstimateRequest:
		promise.Complete(
			fn.Ok[chainsource.ChainSourceResp](
				&chainsource.FeeEstimateResponse{
					SatPerVByte: 5,
				},
			),
		)

	case *chainsource.SubscribeBlocksRequest:
		promise.Complete(
			fn.Ok[chainsource.ChainSourceResp](
				&chainsource.SubscribeBlocksResponse{},
			),
		)

	case *chainsource.RegisterSpendRequest:
		promise.Complete(
			fn.Ok[chainsource.ChainSourceResp](
				&chainsource.RegisterSpendResponse{},
			),
		)

	default:
		promise.Complete(
			fn.Err[chainsource.ChainSourceResp](
				fmt.Errorf("unexpected chainsource msg %T",
					msg),
			),
		)
	}

	return promise.Future()
}

// newRegistryHarness creates a running registry actor with real child actors.
func newRegistryHarness(t *testing.T, proof *recovery.Proof,
	desc *vtxo.Descriptor) (*UnrollRegistryActor, *memRegistryStore,
	*memCheckpointStore, *fakeTxConfirmRef) {

	t.Helper()

	store := newMemRegistryStore()
	checkpoints := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}

	cfg := RegistryConfig{
		Store:         store,
		DeliveryStore: checkpoints,
		ProofAssembler: &mockProofAssembler{
			proof: proof,
		},
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
		TxConfirmRef: txconfirmRef,
		ChainSource: &fakeRegistryChainSourceRef{
			height: 200,
		},
		Wallet: &fakeSweepWallet{},
	}
	registry := newRegistryHarnessWithSpawn(t, cfg)
	t.Cleanup(registry.Stop)

	return registry, store, checkpoints, txconfirmRef
}

// newRegistryHarnessWithSpawn creates a registry actor whose child-spawn path
// uses plain in-memory actors so unit tests do not depend on a full durable
// mailbox implementation.
func newRegistryHarnessWithSpawn(t *testing.T,
	cfg RegistryConfig) *UnrollRegistryActor {

	t.Helper()

	regBehavior := &registryBehavior{
		cfg:        cfg,
		log:        btclog.Disabled,
		active:     make(map[wire.OutPoint]*VTXOUnrollActor),
		pending:    make(map[wire.OutPoint]RegistryRecord),
		persisting: make(map[wire.OutPoint]RegistryRecord),
	}
	registryActor := actor.NewActor(actor.ActorConfig[
		RegistryMsg, RegistryResp,
	]{
		ID:          "unroll-registry-test",
		Behavior:    regBehavior,
		MailboxSize: 64,
	})
	regBehavior.selfRef = registryActor.TellRef()

	regBehavior.spawnFunc = func(_ context.Context, target wire.OutPoint) (
		*VTXOUnrollActor, error) {

		maxFee := cfg.MaxSweepFeeRateSatPerVByte
		childCfg := Config{
			TargetOutpoint:             target,
			ActorID:                    actorIDForTarget(target),
			DeliveryStore:              cfg.DeliveryStore,
			ProofAssembler:             cfg.ProofAssembler,
			VTXOStore:                  cfg.VTXOStore,
			TxConfirmRef:               cfg.TxConfirmRef,
			ChainSource:                cfg.ChainSource,
			Wallet:                     cfg.Wallet,
			RegistryRef:                registryActor.TellRef(),
			MaxSweepFeeRateSatPerVByte: maxFee,
			ExitSpendPolicyResolver:    cfg.ExitSpendPolicyResolver,
		}
		childBehavior := &behavior{
			cfg: childCfg,
			log: btclog.Disabled,
		}
		//nolint:contextcheck // test restore uses t.Context as root
		err := childBehavior.restoreCheckpoint(t.Context())
		if err != nil {
			return nil, err
		}

		//nolint:contextcheck // test child actor owns its own lifecycle
		childActor := actor.NewActor(actor.ActorConfig[Msg, Resp]{
			ID:          actorIDForTarget(target),
			Behavior:    adaptTx(childBehavior),
			MailboxSize: 64,
		})
		childBehavior.selfRef = childActor.TellRef()
		//nolint:contextcheck // test child actor owns its own lifecycle
		childActor.Start()

		return &VTXOUnrollActor{
			ref:  childActor.Ref(),
			stop: childActor.Stop,
		}, nil
	}

	registryActor.Start()

	return &UnrollRegistryActor{
		ref:      registryActor.Ref(),
		registry: registryActor,
		behavior: regBehavior,
	}
}

// newTestUnrollChild creates a lightweight child actor for registry tests that
// need to control StartUnrollRequest behavior.
func newTestUnrollChild(t *testing.T, target wire.OutPoint,
	behavior actor.ActorBehavior[Msg, Resp]) *VTXOUnrollActor {

	t.Helper()

	childActor := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          actorIDForTarget(target),
		Behavior:    behavior,
		MailboxSize: 8,
	})
	childActor.Start()
	t.Cleanup(childActor.Stop)

	return &VTXOUnrollActor{
		ref:  childActor.Ref(),
		stop: childActor.Stop,
	}
}

type terminalDrainRef struct {
	id string

	askStarted chan struct{}
	release    chan struct{}
	startOnce  sync.Once
}

func newTerminalDrainRef(id string) *terminalDrainRef {
	return &terminalDrainRef{
		id:         id,
		askStarted: make(chan struct{}),
		release:    make(chan struct{}),
	}
}

func (r *terminalDrainRef) ID() string {
	return r.id
}

func (r *terminalDrainRef) Tell(context.Context, Msg) error {
	return nil
}

func (r *terminalDrainRef) Ask(ctx context.Context,
	msg Msg) actor.Future[Resp] {

	promise := actor.NewPromise[Resp]()

	go func() {
		if _, ok := msg.(*GetStateRequest); !ok {
			promise.Complete(
				fn.Err[Resp](
					fmt.Errorf("unexpected msg %T", msg),
				),
			)

			return
		}

		r.startOnce.Do(func() {
			close(r.askStarted)
		})

		select {
		case <-r.release:
			promise.Complete(
				fn.Ok[Resp](
					&GetStateResp{
						Phase: PhaseCompleted,
					},
				),
			)

		case <-ctx.Done():
			promise.Complete(fn.Err[Resp](ctx.Err()))
		}
	}()

	return promise.Future()
}

type noopRegistryTellRef struct{}

func (n noopRegistryTellRef) ID() string {
	return "noop-registry"
}

func (n noopRegistryTellRef) Tell(context.Context, RegistryMsg) error {
	return nil
}

// TestRegistryEnsureDedupesSameTarget verifies that the registry creates one
// actor per target and deduplicates repeated starts.
func TestRegistryEnsureDedupesSameTarget(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	registry, store, _, txconfirmRef := newRegistryHarness(t, proof, desc)

	resp, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.True(t, ensureResp.Created)
	require.Equal(t, 1, txconfirmRef.requestCount())

	resp, err = registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	ensureResp, ok = resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.False(t, ensureResp.Created)
	require.Equal(t, 1, txconfirmRef.requestCount())

	record, err := store.GetRecord(t.Context(), proof.TargetOutpoint())
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, PhaseMaterializing, record.Phase)
}

// TestRegistryTerminalNotificationMarksStore verifies that terminal child
// notifications clear the active map and persist terminal control-plane state.
func TestRegistryTerminalNotificationMarksStore(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	registry, store, _, txconfirmRef := newRegistryHarness(t, proof, desc)

	// Confirm the root proof node (a real on-chain footprint), then fail
	// its child node. The job terminates Failed with a confirmed proof tx
	// still on record, so HadOnChainFootprint is true and the failure is
	// NON-recoverable: the exit has begun on-chain, so a repeat Ensure
	// must still dedup against the terminal record rather than re-admit.
	// The recoverable (no-footprint) re-admission case is covered by
	// TestRegistryReadmitsTargetAfterRecoverableFailure.
	rootTxid := proof.RootTxids()[0]
	childTxids, err := proof.ChildTxids(rootTxid)
	require.NoError(t, err)
	require.NotEmpty(t, childTxids)
	txconfirmRef.setImmediateConfirmed(rootTxid, 201)
	txconfirmRef.setImmediateFailed(childTxids[0], "rejected")

	_, err = registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		resp, err := registry.Ref().Ask(
			t.Context(), &GetStatusRequest{
				Outpoint: proof.TargetOutpoint(),
			},
		).Await(t.Context()).Unpack()
		require.NoError(t, err)

		status, ok := resp.(*GetStatusResp)
		require.True(t, ok)

		return status.Found && !status.Active &&
			status.Phase == PhaseFailed
	}, testTimeout, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		record, err := store.GetRecord(
			t.Context(), proof.TargetOutpoint(),
		)
		require.NoError(t, err)

		return record != nil &&
			record.Phase == PhaseFailed &&
			record.FailReason != ""
	}, testTimeout, 10*time.Millisecond)

	record, err := store.GetRecord(t.Context(), proof.TargetOutpoint())
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Contains(t, record.FailReason, "proof tx")
	require.False(
		t, record.RecoverableFailure, "a failure with a confirmed "+
			"proof node has an on-chain footprint and must not "+
			"be recoverable",
	)

	// A repeat EnsureUnroll for the same outpoint after a NON-recoverable
	// termination must not spawn a fresh actor or clobber the stored
	// failure reason; it should return Created=false pointing at the
	// existing actor id so the caller can observe the terminal record.
	storedActorID := record.ActorID

	resp, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.False(t, ensureResp.Created)
	require.Equal(t, storedActorID, ensureResp.ActorID)

	after, err := store.GetRecord(t.Context(), proof.TargetOutpoint())
	require.NoError(t, err)
	require.NotNil(t, after)
	require.Equal(t, PhaseFailed, after.Phase)
	require.Contains(t, after.FailReason, "proof tx")
}

// TestRegistryTerminalNotificationDrainsChildBeforeStop verifies that the
// registry does not cancel a child synchronously from the terminal-notification
// handler. A child notifies the registry from its own message transaction, so
// immediate Stop would cancel the child before it can ack that message.
func TestRegistryTerminalNotificationDrainsChildBeforeStop(t *testing.T) {
	var hash chainhash.Hash
	hash[0] = 1
	target := wire.OutPoint{
		Hash:  hash,
		Index: 0,
	}
	actorID := actorIDForTarget(target)

	ref := newTerminalDrainRef(actorID)
	stopped := make(chan struct{})
	var stopOnce sync.Once
	child := &VTXOUnrollActor{
		ref: ref,
		stop: func() {
			stopOnce.Do(func() {
				close(stopped)
			})
		},
	}

	registry := &registryBehavior{
		cfg: RegistryConfig{
			Store: newMemRegistryStore(),
		},
		selfRef: noopRegistryTellRef{},
		active: map[wire.OutPoint]*VTXOUnrollActor{
			target: child,
		},
		pending: map[wire.OutPoint]RegistryRecord{
			target: {
				TargetOutpoint: target,
				ActorID:        actorID,
				Trigger:        TriggerManual,
				Phase:          PhasePending,
			},
		},
		persisting: make(map[wire.OutPoint]RegistryRecord),
	}

	_, err := registry.handleTerminated(
		t.Context(), &UnrollTerminatedMsg{
			Outpoint: target,
			ActorID:  actorID,
			Phase:    PhaseCompleted,
		},
	).Unpack()
	require.NoError(t, err)

	select {
	case <-ref.askStarted:
	case <-time.After(testTimeout):
		t.Fatal("registry did not probe child before stop")
	}

	select {
	case <-stopped:
		t.Fatal("child stopped before drain probe completed")

	default:
	}

	close(ref.release)

	select {
	case <-stopped:
	case <-time.After(testTimeout):
		t.Fatal("child did not stop after drain probe completed")
	}
}

// TestRegistryRestoreNonTerminal verifies that the registry respawns and
// resumes non-terminal records from the control-plane store.
func TestRegistryRestoreNonTerminal(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	store := newMemRegistryStore()
	checkpoints := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}

	raw, err := encodeCheckpoint(&actorCheckpoint{
		Version: checkpointVersion,
		Height:  150,
		Started: true,
		Trigger: TriggerRestart,
		State: unrollplan.State{
			InFlightTxids: []chainhash.Hash{proof.RootTxids()[0]},
		},
	})
	require.NoError(t, err)

	actorID := actorIDForTarget(proof.TargetOutpoint())
	err = checkpoints.SaveCheckpoint(t.Context(), actor.CheckpointParams{
		ActorID:   actorID,
		StateType: checkpointStateType,
		StateData: raw,
		Version:   checkpointVersion,
	})
	require.NoError(t, err)

	err = store.UpsertRecord(t.Context(), RegistryRecord{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        actorID,
		Trigger:        TriggerRestart,
		Phase:          PhaseMaterializing,
	})
	require.NoError(t, err)

	registry := newRegistryHarnessWithSpawn(t, RegistryConfig{
		Store:          store,
		DeliveryStore:  checkpoints,
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   txconfirmRef,
		ChainSource:    &fakeRegistryChainSourceRef{height: 201},
		Wallet:         &fakeSweepWallet{},
	})
	t.Cleanup(registry.Stop)

	err = registry.RestoreNonTerminal(t.Context())
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		txid := proof.RootTxids()[0]

		return txconfirmRef.requestCountForTxid(txid) == 1
	}, testTimeout, 10*time.Millisecond)

	resp, err := registry.Ref().Ask(t.Context(), &GetStatusRequest{
		Outpoint: proof.TargetOutpoint(),
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	status, ok := resp.(*GetStatusResp)
	require.True(t, ok)
	require.True(t, status.Found)
	require.True(t, status.Active)
	require.Equal(t, PhaseMaterializing, status.Phase)
}

// TestRegistryStatusUsesCachedActiveRecord verifies that live status probes do
// not enqueue read-only GetStateRequest messages into the child actor.
func TestRegistryStatusUsesCachedActiveRecord(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	store := newMemRegistryStore()
	var stateRequests atomic.Int32

	registry := newRegistryHarnessWithSpawn(t, RegistryConfig{
		Store:          store,
		DeliveryStore:  newMemCheckpointStore(),
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   &fakeTxConfirmRef{},
		ChainSource:    &fakeRegistryChainSourceRef{height: 200},
		Wallet:         &fakeSweepWallet{},
	})
	t.Cleanup(registry.Stop)

	registry.behavior.spawnFunc = func(_ context.Context,
		target wire.OutPoint) (*VTXOUnrollActor, error) {

		behavior := actor.NewFunctionBehavior(
			func(_ context.Context, msg Msg) fn.Result[Resp] {
				switch msg.(type) {
				case *StartUnrollRequest:
					return fn.Ok[Resp](&AckResp{})

				case *GetStateRequest:
					stateRequests.Add(1)

					return fn.Ok[Resp](&GetStateResp{
						Started: true,
						Trigger: TriggerManual,
						Phase:   PhaseMaterializing,
					})

				default:
					return fn.Err[Resp](
						fmt.Errorf("unexpected msg %T",
							msg),
					)
				}
			},
		)

		// Test children are owned by t.Cleanup after creation.
		//nolint:contextcheck
		return newTestUnrollChild(t, target, behavior), nil
	}

	resp, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.True(t, ensureResp.Created)

	require.Eventually(t, func() bool {
		return stateRequests.Load() == 1
	}, testTimeout, 10*time.Millisecond)

	for i := 0; i < 3; i++ {
		resp, err := registry.Ref().Ask(
			t.Context(), &GetStatusRequest{
				Outpoint: proof.TargetOutpoint(),
			},
		).Await(t.Context()).Unpack()
		require.NoError(t, err)

		status, ok := resp.(*GetStatusResp)
		require.True(t, ok)
		require.True(t, status.Found)
		require.True(t, status.Active)
		require.Nil(t, status.State)
		require.Equal(t, PhaseMaterializing, status.Phase)
	}

	require.EqualValues(t, 1, stateRequests.Load())
}

// TestRegistryEnsureFailsClosedOnInitialPersistFailure verifies the
// fail-closed admission contract: if the initial control-plane upsert
// fails, EnsureUnroll surfaces that error, no child is left in the
// active map, and the caller can retry once the store is healthy.
// Prior behavior returned Created=true even when the record was not yet
// durable, opening a crash window where a child would be orphaned on
// restart (RestoreNonTerminal only walks the durable store).
func TestRegistryEnsureFailsClosedOnInitialPersistFailure(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())

	// Fail the very first UpsertRecord call, then succeed.
	store := newFlakyRegistryStore(1)
	checkpoints := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}

	registry := newRegistryHarnessWithSpawn(t, RegistryConfig{
		Store:          store,
		DeliveryStore:  checkpoints,
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   txconfirmRef,
		ChainSource:    &fakeRegistryChainSourceRef{height: 200},
		Wallet:         &fakeSweepWallet{},
	})
	t.Cleanup(registry.Stop)

	// The admission write hits the injected failure so EnsureUnroll must
	// surface that error rather than return Created=true with an
	// unpersisted in-memory child.
	_, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(t.Context()).Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "persist unroll record")

	// No record should have been persisted and no child should remain
	// accessible via status queries.
	record, err := store.GetRecord(t.Context(), proof.TargetOutpoint())
	require.NoError(t, err)
	require.Nil(t, record)

	statusResp, err := registry.Ref().Ask(t.Context(), &GetStatusRequest{
		Outpoint: proof.TargetOutpoint(),
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	gotStatus, ok := statusResp.(*GetStatusResp)
	require.True(t, ok)
	require.False(t, gotStatus.Found)

	// A retry against the now-healthy store must succeed and land a
	// durable record so RestoreNonTerminal could pick it up on reboot.
	resp, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.True(t, ensureResp.Created)

	require.Eventually(t, func() bool {
		record, err := store.GetRecord(
			t.Context(), proof.TargetOutpoint(),
		)
		require.NoError(t, err)

		return record != nil
	}, testTimeout, 10*time.Millisecond)
}

// TestRegistryEnsurePersistsBeforeAck locks in the invariant that the
// control-plane record is durable in the store before EnsureUnroll
// returns Created=true, so a crash immediately after admission does not
// orphan the child on restart (RestoreNonTerminal reads from the durable
// store, not in-memory state).
func TestRegistryEnsurePersistsBeforeAck(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	registry, store, _, _ := newRegistryHarness(t, proof, desc)

	resp, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.True(t, ensureResp.Created)

	// The record must already be durable at this point; no Eventually
	// loop: a sync-persist regression would show up as a transient
	// absence here.
	record, err := store.GetRecord(t.Context(), proof.TargetOutpoint())
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, ensureResp.ActorID, record.ActorID)
	require.Equal(t, TriggerManual, record.Trigger)
}

// TestRegistryEnsurePersistsExitPolicyIdentity verifies custom admission
// policy identity survives the registry and child actor start path. Without
// this, custom recovery callers would persist an unroll job that restarts as a
// standard VTXO timeout sweep.
func TestRegistryEnsurePersistsExitPolicyIdentity(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	registry, store, _, _ := newRegistryHarness(t, proof, desc)

	const (
		policyKind ExitPolicyKind = "vhtlc_claim"
		policyRef                 = "recovery-policy-ref"
	)

	resp, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint:       proof.TargetOutpoint(),
		Trigger:        TriggerManual,
		ExitPolicyKind: policyKind,
		ExitPolicyRef:  policyRef,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.True(t, ensureResp.Created)

	record, err := store.GetRecord(t.Context(), proof.TargetOutpoint())
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, policyKind, record.ExitPolicyKind)
	require.Equal(t, policyRef, record.ExitPolicyRef)

	status, err := registry.Ref().Ask(
		t.Context(), &GetStatusRequest{
			Outpoint: proof.TargetOutpoint(),
		},
	).Await(t.Context()).Unpack()
	require.NoError(t, err)

	statusResp, ok := status.(*GetStatusResp)
	require.True(t, ok)
	require.Equal(t, policyKind, statusResp.ExitPolicyKind)
	require.Equal(t, policyRef, statusResp.ExitPolicyRef)
}

// TestRegistryEnsureStartsChildAfterCallerCancellation verifies that once the
// pending control-plane row exists, caller cancellation does not leak into the
// durable child's first StartUnrollRequest.
func TestRegistryEnsureStartsChildAfterCallerCancellation(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())

	ctx, cancel := context.WithCancel(t.Context())
	store := &cancelOnPendingRegistryStore{
		memRegistryStore: newMemRegistryStore(),
		cancel:           cancel,
	}

	started := make(chan struct{}, 1)
	startCtxErr := make(chan error, 1)
	registry := newRegistryHarnessWithSpawn(t, RegistryConfig{
		Store:          store,
		DeliveryStore:  newMemCheckpointStore(),
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   &fakeTxConfirmRef{},
		ChainSource:    &fakeRegistryChainSourceRef{height: 200},
		Wallet:         &fakeSweepWallet{},
	})
	t.Cleanup(registry.Stop)

	registry.behavior.spawnFunc = func(_ context.Context,
		target wire.OutPoint) (*VTXOUnrollActor, error) {

		behavior := actor.NewFunctionBehavior(
			func(ctx context.Context, msg Msg) fn.Result[Resp] {
				switch msg.(type) {
				case *StartUnrollRequest:
					startCtxErr <- ctx.Err()
					started <- struct{}{}

					return fn.Ok[Resp](&AckResp{})

				case *GetStateRequest:
					return fn.Ok[Resp](&GetStateResp{
						Started: true,
						Trigger: TriggerManual,
						Phase:   PhasePending,
					})

				default:
					return fn.Err[Resp](
						fmt.Errorf("unexpected msg %T",
							msg),
					)
				}
			},
		)

		// Test children are owned by t.Cleanup after creation.
		//nolint:contextcheck
		return newTestUnrollChild(t, target, behavior), nil
	}

	resp, err := registry.Ref().Ask(ctx, &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(context.Background()).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.True(t, ensureResp.Created)

	record, err := store.GetRecord(t.Context(), proof.TargetOutpoint())
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, PhasePending, record.Phase)

	select {
	case <-started:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for child start")
	}

	require.NoError(t, <-startCtxErr)
}

// TestRegistryEnsureMarksRealStartErrorFailed verifies that the pending-row
// safeguard does not hide non-cancellation child start failures.
func TestRegistryEnsureMarksRealStartErrorFailed(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	store := newMemRegistryStore()

	registry := newRegistryHarnessWithSpawn(t, RegistryConfig{
		Store:          store,
		DeliveryStore:  newMemCheckpointStore(),
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   &fakeTxConfirmRef{},
		ChainSource:    &fakeRegistryChainSourceRef{height: 200},
		Wallet:         &fakeSweepWallet{},
	})
	t.Cleanup(registry.Stop)

	registry.behavior.spawnFunc = func(_ context.Context,
		target wire.OutPoint) (*VTXOUnrollActor, error) {

		behavior := actor.NewFunctionBehavior(
			func(_ context.Context, msg Msg) fn.Result[Resp] {
				switch msg.(type) {
				case *StartUnrollRequest:
					return fn.Err[Resp](
						errors.New("start boom"),
					)

				case *GetStateRequest:
					return fn.Ok[Resp](&GetStateResp{
						Started: false,
						Phase:   PhasePending,
					})

				default:
					return fn.Err[Resp](
						fmt.Errorf("unexpected msg %T",
							msg),
					)
				}
			},
		)

		// Test children are owned by t.Cleanup after creation.
		//nolint:contextcheck
		return newTestUnrollChild(t, target, behavior), nil
	}

	_, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(t.Context()).Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "start child")
	require.Contains(t, err.Error(), "start boom")

	record, err := store.GetRecord(t.Context(), proof.TargetOutpoint())
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, PhaseFailed, record.Phase)
	require.Contains(t, record.FailReason, "start boom")
}

// TestRegistryTerminalPersistRetriesUntilDurable verifies that a
// terminal record that transiently fails to persist is retried by the
// async writer loop and eventually lands in the control-plane store.
// The fail-closed admission contract handles the initial Pending write
// synchronously, but terminal updates stay on the retry path so a flaky
// store on the terminal write does not lose the failure record.
func TestRegistryTerminalPersistRetriesUntilDurable(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())

	// Fail the first TERMINAL upsert, then succeed. The non-terminal
	// admission write is never failed so the child boots cleanly and
	// the fail-closed admission contract is unaffected.
	store := newTerminalFlakyRegistryStore(1)
	checkpoints := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}

	registry := newRegistryHarnessWithSpawn(t, RegistryConfig{
		Store:          store,
		DeliveryStore:  checkpoints,
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   txconfirmRef,
		ChainSource:    &fakeRegistryChainSourceRef{height: 200},
		Wallet:         &fakeSweepWallet{},
	})
	t.Cleanup(registry.Stop)

	_, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	// Drive the child to terminal AFTER admission so the first terminal
	// upsert hits the injected failure and the retry path must land
	// the record.
	txconfirmRef.emitFailed(t, 0, proof.RootTxids()[0], "rejected")

	require.Eventually(t, func() bool {
		record, err := store.GetRecord(
			t.Context(), proof.TargetOutpoint(),
		)
		require.NoError(t, err)

		return record != nil &&
			record.Phase == PhaseFailed &&
			record.FailReason != ""
	}, testTimeout, 10*time.Millisecond)

	record, err := store.GetRecord(t.Context(), proof.TargetOutpoint())
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Contains(t, record.FailReason, "rejected")
}

// TestRegistryStatusFallsBackToPendingTerminalRecord verifies that the
// registry can still answer status queries from memory after a child
// has terminated when every terminal control-plane upsert fails. The
// initial admission write is fail-closed, so the non-terminal phase
// upsert must succeed for admission to complete; only the terminal
// retries stay rejected, exercising the in-memory pending fallback.
func TestRegistryStatusFallsBackToPendingTerminalRecord(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	store := newAlwaysFailUpsertRegistryStore()
	checkpoints := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}

	registry := newRegistryHarnessWithSpawn(t, RegistryConfig{
		Store:          store,
		DeliveryStore:  checkpoints,
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   txconfirmRef,
		ChainSource:    &fakeRegistryChainSourceRef{height: 200},
		Wallet:         &fakeSweepWallet{},
	})
	t.Cleanup(registry.Stop)

	_, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	// Drive the child to terminal AFTER admission so only the terminal
	// upsert hits the always-fail injection.
	txconfirmRef.emitFailed(t, 0, proof.RootTxids()[0], "rejected")

	require.Eventually(t, func() bool {
		resp, err := registry.Ref().Ask(
			t.Context(), &GetStatusRequest{
				Outpoint: proof.TargetOutpoint(),
			},
		).Await(t.Context()).Unpack()
		require.NoError(t, err)

		status, ok := resp.(*GetStatusResp)
		require.True(t, ok)
		if !status.Found || status.Active ||
			status.Phase != PhaseFailed {
			return false
		}

		// The durable store reflects the initial admission write
		// (non-terminal) because every terminal upsert is rejected
		// by the injected store; the registry must still report
		// PhaseFailed from the in-memory pending snapshot so callers
		// see the real state despite the stalled writeback.
		record, err := store.GetRecord(
			t.Context(), proof.TargetOutpoint(),
		)
		require.NoError(t, err)

		return record != nil && !record.IsTerminal() &&
			status.ActorID != "" &&
			status.FailReason != ""
	}, testTimeout, 10*time.Millisecond)
}

// TestRegistryTerminalStatusRemainsQueryableWhilePersistBlocked verifies
// that the registry can still report a fast terminal failure while the
// control store is blocked on persisting the terminal row. The initial
// admission write is fail-closed and non-terminal, so it passes through
// the blocking store inline; only the terminal write is gated.
func TestRegistryTerminalStatusRemainsQueryableWhilePersistBlocked(
	t *testing.T) {

	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	store := newBlockingRegistryStore()
	checkpoints := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}

	registry := newRegistryHarnessWithSpawn(t, RegistryConfig{
		Store:          store,
		DeliveryStore:  checkpoints,
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   txconfirmRef,
		ChainSource:    &fakeRegistryChainSourceRef{height: 200},
		Wallet:         &fakeSweepWallet{},
	})
	t.Cleanup(registry.Stop)

	resp, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.True(t, ensureResp.Created)

	// Drive the child to terminal AFTER admission so only the terminal
	// upsert is gated by the blocking store.
	txconfirmRef.emitFailed(t, 0, proof.RootTxids()[0], "rejected")

	select {
	case <-store.started:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for blocked registry persist")
	}

	require.Eventually(t, func() bool {
		resp, err := registry.Ref().Ask(
			t.Context(), &GetStatusRequest{
				Outpoint: proof.TargetOutpoint(),
			},
		).Await(t.Context()).Unpack()
		require.NoError(t, err)

		status, ok := resp.(*GetStatusResp)
		require.True(t, ok)

		return status.Found && status.Phase == PhaseFailed
	}, testTimeout, 10*time.Millisecond)

	close(store.release)

	require.Eventually(t, func() bool {
		record, err := store.GetRecord(
			t.Context(), proof.TargetOutpoint(),
		)
		require.NoError(t, err)

		return record != nil && record.Phase == PhaseFailed
	}, testTimeout, 10*time.Millisecond)
}

// TestRegistryRestoreFailureLeavesRecordRetryable verifies that a
// transient failure during RestoreNonTerminal fails the boot attempt
// without marking the durable record terminal: a subsequent
// RestoreNonTerminal call (e.g. on the next daemon boot, after the
// transient ChainSource / DB issue is resolved) must still find and
// resume the job. This is the regression guard for issue #381
// ("Restore failure permanently disables unroll recovery") and #400
// ("restore failure is only logged").
func TestRegistryRestoreFailureLeavesRecordRetryable(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	store := newMemRegistryStore()
	checkpoints := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}

	actorID := actorIDForTarget(proof.TargetOutpoint())
	err := store.UpsertRecord(t.Context(), RegistryRecord{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        actorID,
		Trigger:        TriggerRestart,
		Phase:          PhaseMaterializing,
	})
	require.NoError(t, err)

	registry := newRegistryHarnessWithSpawn(t, RegistryConfig{
		Store:          store,
		DeliveryStore:  checkpoints,
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   txconfirmRef,
		ChainSource:    &fakeRegistryChainSourceRef{height: 201},
		Wallet:         &fakeSweepWallet{},
	})
	t.Cleanup(registry.Stop)

	// First boot: install a spawnFunc that returns a child whose
	// ResumeUnrollRequest fails (simulating a transient ChainSource
	// outage on SubscribeBlocks / RegisterSpend during resume).
	var attempts atomic.Int32
	failingSpawn := func(_ context.Context, target wire.OutPoint) (
		*VTXOUnrollActor, error) {

		attempts.Add(1)
		behavior := actor.NewFunctionBehavior(
			func(_ context.Context, msg Msg) fn.Result[Resp] {
				if _, ok := msg.(*ResumeUnrollRequest); ok {
					err := errors.New("transient chain " +
						"outage")

					return fn.Err[Resp](err)
				}

				return fn.Err[Resp](
					fmt.Errorf("unexpected msg %T", msg),
				)
			},
		)

		// Test children are owned by t.Cleanup after creation.
		//nolint:contextcheck
		return newTestUnrollChild(t, target, behavior), nil
	}
	registry.behavior.spawnFunc = failingSpawn

	// RestoreNonTerminal must return an error so daemon startup fails
	// closed, but must NOT mark the record terminal; the durable record
	// stays non-terminal so a future retry path can pick it up.
	err = registry.RestoreNonTerminal(t.Context())
	require.ErrorContains(t, err, "restore 1 unroll job")
	require.EqualValues(t, 1, attempts.Load())

	record, err := store.GetRecord(t.Context(), proof.TargetOutpoint())
	require.NoError(t, err)
	require.NotNil(t, record)
	require.False(
		t, record.IsTerminal(),
		"restore failure must leave record non-terminal",
	)
	require.Equal(t, PhaseMaterializing, record.Phase)

	// Second boot path: swap in a healthy spawnFunc that completes
	// the ResumeUnrollRequest and verify RestoreNonTerminal now
	// succeeds. The durable record must still be visible to
	// ListNonTerminalRecords.
	healthySpawn := func(_ context.Context, target wire.OutPoint) (
		*VTXOUnrollActor, error) {

		attempts.Add(1)
		behavior := actor.NewFunctionBehavior(
			func(_ context.Context, msg Msg) fn.Result[Resp] {
				switch msg.(type) {
				case *ResumeUnrollRequest:
					return fn.Ok[Resp](&AckResp{})

				case *GetStateRequest:
					return fn.Ok[Resp](&GetStateResp{
						Started: true,
						Trigger: TriggerRestart,
						Phase:   PhaseMaterializing,
					})

				default:
					return fn.Err[Resp](
						fmt.Errorf("unexpected msg %T",
							msg),
					)
				}
			},
		)

		// Test children are owned by t.Cleanup after creation.
		//nolint:contextcheck
		return newTestUnrollChild(t, target, behavior), nil
	}
	registry.behavior.spawnFunc = healthySpawn

	err = registry.RestoreNonTerminal(t.Context())
	require.NoError(t, err)
	require.EqualValues(
		t, 2, attempts.Load(),
		"second RestoreNonTerminal must respawn the child",
	)

	// The job is now active and visible via GetStatus.
	resp, err := registry.Ref().Ask(t.Context(), &GetStatusRequest{
		Outpoint: proof.TargetOutpoint(),
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	status, ok := resp.(*GetStatusResp)
	require.True(t, ok)
	require.True(t, status.Found)
	require.True(t, status.Active)
	require.Equal(t, PhaseMaterializing, status.Phase)
}

// TestRegistryLateAdmissionFailureMarksFailed verifies that when the
// registry's synchronous admission wait times out, the eventual child
// start result is still observed and a late deterministic start failure
// becomes a terminal registry record instead of leaving PhasePending
// forever.
func TestRegistryLateAdmissionFailureMarksFailed(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	store := newMemRegistryStore()

	registry := newRegistryHarnessWithSpawn(t, RegistryConfig{
		Store:          store,
		DeliveryStore:  newMemCheckpointStore(),
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   &fakeTxConfirmRef{},
		ChainSource:    &fakeRegistryChainSourceRef{height: 200},
		Wallet:         &fakeSweepWallet{},
	})
	t.Cleanup(registry.Stop)

	target := proof.TargetOutpoint()
	child := newTestUnrollChild(
		t, target, actor.NewFunctionBehavior(
			func(context.Context, Msg) fn.Result[Resp] {
				return fn.Ok[Resp](&AckResp{})
			},
		),
	)

	record := RegistryRecord{
		TargetOutpoint: target,
		ActorID:        child.Ref().ID(),
		Trigger:        TriggerManual,
		Phase:          PhasePending,
	}
	require.NoError(t, store.UpsertRecord(t.Context(), record))
	registry.behavior.active[target] = child
	registry.behavior.pending[target] = cloneRegistryRecord(record)

	promise := actor.NewPromise[Resp]()
	registry.behavior.watchChildAdmissionResult(
		t.Context(), target, child, promise.Future(),
	)
	promise.Complete(fn.Err[Resp](errors.New("late start boom")))

	require.Eventually(t, func() bool {
		record, err := store.GetRecord(t.Context(), target)
		require.NoError(t, err)

		return record != nil &&
			record.Phase == PhaseFailed &&
			strings.Contains(record.FailReason, "late start boom")
	}, testTimeout, 10*time.Millisecond)
}

// TestRegistryEnsureRestoresFailedNonTerminalRecord verifies that when a
// non-terminal record exists in the durable store but no child is
// active (because a prior RestoreNonTerminal hit a transient error and
// left the record retryable), a fresh EnsureUnrollRequest from the
// chain resolver or RPC layer kicks off an inline restore instead of
// silently returning Created=false with a dormant job. This is the
// second half of the issue #381 fix: handleEnsure used to short-circuit
// on any existing record without checking whether the in-memory child
// was actually live.
func TestRegistryEnsureRestoresFailedNonTerminalRecord(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	store := newMemRegistryStore()
	checkpoints := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}

	actorID := actorIDForTarget(proof.TargetOutpoint())
	err := store.UpsertRecord(t.Context(), RegistryRecord{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        actorID,
		Trigger:        TriggerRestart,
		Phase:          PhaseMaterializing,
	})
	require.NoError(t, err)

	registry := newRegistryHarnessWithSpawn(t, RegistryConfig{
		Store:          store,
		DeliveryStore:  checkpoints,
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   txconfirmRef,
		ChainSource:    &fakeRegistryChainSourceRef{height: 201},
		Wallet:         &fakeSweepWallet{},
	})
	t.Cleanup(registry.Stop)

	// Force the prior RestoreNonTerminal to "fail" by simply skipping
	// it: leave the store record in place but never wire up an active
	// child. EnsureUnroll must detect the gap and restore inline.
	var resumes atomic.Int32
	registry.behavior.spawnFunc = func(_ context.Context,
		target wire.OutPoint) (*VTXOUnrollActor, error) {

		behavior := actor.NewFunctionBehavior(
			func(_ context.Context, msg Msg) fn.Result[Resp] {
				switch msg.(type) {
				case *ResumeUnrollRequest:
					resumes.Add(1)

					return fn.Ok[Resp](&AckResp{})

				case *GetStateRequest:
					return fn.Ok[Resp](&GetStateResp{
						Started: true,
						Trigger: TriggerRestart,
						Phase:   PhaseMaterializing,
					})

				default:
					return fn.Err[Resp](
						fmt.Errorf("unexpected msg %T",
							msg),
					)
				}
			},
		)

		// Test children are owned by t.Cleanup after creation.
		//nolint:contextcheck
		return newTestUnrollChild(t, target, behavior), nil
	}

	// Caller asks Ensure for the same outpoint. The pre-existing
	// non-terminal record + no active child must trigger inline
	// restore via ResumeUnrollRequest. Created=false because the
	// job was admitted in a previous boot.
	resp, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerCriticalExpiry,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.False(
		t, ensureResp.Created,
		"existing record must surface as Created=false",
	)
	require.Equal(t, actorID, ensureResp.ActorID)
	require.EqualValues(
		t, 1, resumes.Load(),
		"EnsureUnroll on a non-terminal record with no active "+
			"child must trigger inline resume",
	)

	// The job is now active.
	statusResp, err := registry.Ref().Ask(t.Context(), &GetStatusRequest{
		Outpoint: proof.TargetOutpoint(),
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	status, ok := statusResp.(*GetStatusResp)
	require.True(t, ok)
	require.True(t, status.Active)
}

// TestRegistryEnsureRetriesAfterInlineRestoreFailure verifies that an
// inline restore failure inside handleEnsure does not strand the job:
// a subsequent EnsureUnroll on the same outpoint must attempt restore
// again rather than short-circuiting on the dormant non-terminal
// record. Combined with the no-mark-terminal behavior in
// restoreNonTerminal, this means a transient backend outage is fully
// recoverable both on the next boot AND via a follow-up Ensure within
// the same boot.
func TestRegistryEnsureRetriesAfterInlineRestoreFailure(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	store := newMemRegistryStore()
	checkpoints := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}

	actorID := actorIDForTarget(proof.TargetOutpoint())
	err := store.UpsertRecord(t.Context(), RegistryRecord{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        actorID,
		Trigger:        TriggerRestart,
		Phase:          PhaseMaterializing,
	})
	require.NoError(t, err)

	registry := newRegistryHarnessWithSpawn(t, RegistryConfig{
		Store:          store,
		DeliveryStore:  checkpoints,
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   txconfirmRef,
		ChainSource:    &fakeRegistryChainSourceRef{height: 201},
		Wallet:         &fakeSweepWallet{},
	})
	t.Cleanup(registry.Stop)

	// First Ensure: ResumeUnrollRequest fails (transient).
	var attempts atomic.Int32
	registry.behavior.spawnFunc = func(_ context.Context,
		target wire.OutPoint) (*VTXOUnrollActor, error) {

		attempts.Add(1)
		behavior := actor.NewFunctionBehavior(
			func(_ context.Context, msg Msg) fn.Result[Resp] {
				if _, ok := msg.(*ResumeUnrollRequest); ok {
					return fn.Err[Resp](
						errors.New(
							"transient resume " +
								"failure"),
					)
				}

				return fn.Err[Resp](
					fmt.Errorf("unexpected msg %T", msg),
				)
			},
		)

		// Test children are owned by t.Cleanup after creation.
		//nolint:contextcheck
		return newTestUnrollChild(t, target, behavior), nil
	}

	_, err = registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerCriticalExpiry,
	}).Await(t.Context()).Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "restore existing record")

	// The durable record must NOT have been marked terminal by the
	// failed inline restore.
	record, err := store.GetRecord(t.Context(), proof.TargetOutpoint())
	require.NoError(t, err)
	require.NotNil(t, record)
	require.False(t, record.IsTerminal())

	// Second Ensure with a healthy spawn must succeed.
	registry.behavior.spawnFunc = func(_ context.Context,
		target wire.OutPoint) (*VTXOUnrollActor, error) {

		attempts.Add(1)
		behavior := actor.NewFunctionBehavior(
			func(_ context.Context, msg Msg) fn.Result[Resp] {
				switch msg.(type) {
				case *ResumeUnrollRequest:
					return fn.Ok[Resp](&AckResp{})

				case *GetStateRequest:
					return fn.Ok[Resp](&GetStateResp{
						Started: true,
						Trigger: TriggerRestart,
						Phase:   PhaseMaterializing,
					})

				default:
					return fn.Err[Resp](
						fmt.Errorf("unexpected msg %T",
							msg),
					)
				}
			},
		)

		// Test children are owned by t.Cleanup after creation.
		//nolint:contextcheck
		return newTestUnrollChild(t, target, behavior), nil
	}

	resp, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerCriticalExpiry,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.False(t, ensureResp.Created)
	require.EqualValues(t, 2, attempts.Load())
}

// TestRegistryEnsureRejectsInlineRestoreWhenResolverMissingKind verifies that
// the same fail-secure policy-kind gate used during boot restore also covers
// the inline restore path in handleEnsure. Without this, a dormant persisted
// custom-policy job could bypass startup validation and only fail later inside
// the child actor's sweep loop.
func TestRegistryEnsureRejectsInlineRestoreWhenResolverMissingKind(
	t *testing.T) {

	proof := buildLinearProof(t)
	store := newMemRegistryStore()
	checkpoints := newMemCheckpointStore()

	err := store.UpsertRecord(t.Context(), RegistryRecord{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        actorIDForTarget(proof.TargetOutpoint()),
		Trigger:        TriggerRestart,
		Phase:          PhaseMaterializing,
		ExitPolicyKind: "vhtlc_claim",
		ExitPolicyRef:  "recovery-1",
	})
	require.NoError(t, err)

	registry := newRegistryHarnessWithSpawn(t, RegistryConfig{
		Store:         store,
		DeliveryStore: checkpoints,
		ChainSource: &fakeRegistryChainSourceRef{
			height: 201,
		},
		ExitSpendPolicyResolver: &fixedKindResolver{
			kinds: map[ExitPolicyKind]struct{}{
				StandardVTXOTimeoutExitPolicyKind: {},
			},
		},
	})
	t.Cleanup(registry.Stop)

	var spawns atomic.Int32
	registry.behavior.spawnFunc = func(_ context.Context,
		target wire.OutPoint) (*VTXOUnrollActor, error) {

		spawns.Add(1)

		return nil, fmt.Errorf("unexpected spawn for %s", target)
	}

	_, err = registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerCriticalExpiry,
	}).Await(t.Context()).Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "validate existing record")
	require.Contains(t, err.Error(), "vhtlc_claim")
	require.Zero(t, spawns.Load())
}

// TestRegistryRestoreNonTerminalDispatchedThroughActor verifies that
// RestoreNonTerminal serializes its r.active / r.pending mutations with
// concurrent Ensure / GetStatus traffic by going through the registry
// actor's Receive loop. Running this test under -race is the actual
// guard: any direct mutation outside the actor goroutine while another
// Ensure is in flight would trip the detector.
func TestRegistryRestoreNonTerminalDispatchedThroughActor(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	store := newMemRegistryStore()
	checkpoints := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}

	actorID := actorIDForTarget(proof.TargetOutpoint())
	err := store.UpsertRecord(t.Context(), RegistryRecord{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        actorID,
		Trigger:        TriggerRestart,
		Phase:          PhaseMaterializing,
	})
	require.NoError(t, err)

	registry := newRegistryHarnessWithSpawn(t, RegistryConfig{
		Store:          store,
		DeliveryStore:  checkpoints,
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   txconfirmRef,
		ChainSource:    &fakeRegistryChainSourceRef{height: 201},
		Wallet:         &fakeSweepWallet{},
	})
	t.Cleanup(registry.Stop)

	registry.behavior.spawnFunc = func(_ context.Context,
		target wire.OutPoint) (*VTXOUnrollActor, error) {

		behavior := actor.NewFunctionBehavior(
			func(_ context.Context, msg Msg) fn.Result[Resp] {
				switch msg.(type) {
				case *ResumeUnrollRequest:
					return fn.Ok[Resp](&AckResp{})

				case *StartUnrollRequest:
					return fn.Ok[Resp](&AckResp{})

				case *GetStateRequest:
					return fn.Ok[Resp](&GetStateResp{
						Started: true,
						Trigger: TriggerRestart,
						Phase:   PhaseMaterializing,
					})

				default:
					return fn.Err[Resp](
						fmt.Errorf("unexpected msg %T",
							msg),
					)
				}
			},
		)

		// Test children are owned by t.Cleanup after creation.
		//nolint:contextcheck
		return newTestUnrollChild(t, target, behavior), nil
	}

	// Drive RestoreNonTerminal and a concurrent GetStatus probe at the
	// same time. Both end up in the actor mailbox; -race catches any
	// behavior-side state still mutated outside the goroutine.
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()

		require.NoError(t, registry.RestoreNonTerminal(t.Context()))
	}()

	go func() {
		defer wg.Done()

		// A GetStatus probe is a read-only message that lands on the
		// same mailbox, so the registry serializes it against the
		// restore turn.
		_, _ = registry.Ref().Ask(t.Context(), &GetStatusRequest{
			Outpoint: proof.TargetOutpoint(),
		}).Await(t.Context()).Unpack()
	}()

	wg.Wait()

	// After restore, the record is active.
	resp, err := registry.Ref().Ask(t.Context(), &GetStatusRequest{
		Outpoint: proof.TargetOutpoint(),
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	status, ok := resp.(*GetStatusResp)
	require.True(t, ok)
	require.True(t, status.Found)
	require.True(t, status.Active)
	require.Equal(t, PhaseMaterializing, status.Phase)
}

// recordingExitSpendPolicyResolver is a test-only ExitSpendPolicyResolver
// that records every call so tests can assert the registry forwarded the
// resolver from RegistryConfig down into each spawned VTXOUnrollActor.
type recordingExitSpendPolicyResolver struct {
	mu       sync.Mutex
	requests []ExitSpendPolicyRequest
}

// ResolveExitSpendPolicy implements ExitSpendPolicyResolver. The resolver
// always returns an error so tests that drive a full sweep can confirm the
// custom resolver, not the default standard resolver, was consulted.
func (r *recordingExitSpendPolicyResolver) ResolveExitSpendPolicy(
	_ context.Context, req ExitSpendPolicyRequest) (ExitSpendPolicy,
	error) {

	r.mu.Lock()
	r.requests = append(r.requests, req)
	r.mu.Unlock()

	return nil, errors.New("recording resolver: not implemented")
}

// TestRegistryForwardsExitSpendPolicyResolver verifies that the
// ExitSpendPolicyResolver configured on the registry is forwarded into the
// Config of every spawned VTXOUnrollActor. Without this wiring the registry
// silently falls back to the standard resolver and any non-standard policy
// kind (e.g. vhtlc_claim) fails closed at sweep time.
func TestRegistryForwardsExitSpendPolicyResolver(t *testing.T) {
	resolver := &recordingExitSpendPolicyResolver{}

	r := &registryBehavior{
		cfg: RegistryConfig{
			ExitSpendPolicyResolver: resolver,
		},
	}

	target := wire.OutPoint{Index: 7}
	childCfg := r.childConfig(target)

	require.Equal(
		t, ExitSpendPolicyResolver(resolver),
		childCfg.ExitSpendPolicyResolver,
	)
	require.Equal(t, target, childCfg.TargetOutpoint)
}

// TestRegistryChildResolverDefaultsToNil documents the safe default: when
// RegistryConfig.ExitSpendPolicyResolver is unset, the forwarded child Config
// also carries nil so the per-actor fallback to the standard resolver kicks
// in. Daemons that arm non-standard policies must wire a resolver explicitly.
func TestRegistryChildResolverDefaultsToNil(t *testing.T) {
	r := &registryBehavior{cfg: RegistryConfig{}}

	target := wire.OutPoint{Index: 9}
	childCfg := r.childConfig(target)

	require.Nil(t, childCfg.ExitSpendPolicyResolver)
}

// fixedKindResolver implements ExitSpendPolicyResolver +
// ResolverKindSupport for the boot-admission check tests.
type fixedKindResolver struct {
	kinds map[ExitPolicyKind]struct{}
}

// SupportsKind implements ResolverKindSupport.
func (f *fixedKindResolver) SupportsKind(kind ExitPolicyKind) bool {
	_, ok := f.kinds[kind]

	return ok
}

// ResolveExitSpendPolicy returns a non-implemented sentinel since the test
// only exercises the admission gate.
func (f *fixedKindResolver) ResolveExitSpendPolicy(_ context.Context,
	_ ExitSpendPolicyRequest) (ExitSpendPolicy, error) {

	return nil, errors.New("fixed kind resolver: not implemented")
}

// TestValidateExitPolicyIdentity exercises the (kind, ref) pair invariant
// the registry admission gate enforces. Standard timeout jobs must omit the
// ref; non-standard kinds must carry one.
func TestValidateExitPolicyIdentity(t *testing.T) {
	cases := []struct {
		name    string
		kind    ExitPolicyKind
		ref     string
		wantErr string
	}{
		{
			name: "empty kind defaults to standard, no ref ok",
			kind: "",
		},
		{
			name:    "empty kind defaults to standard, ref errors",
			kind:    "",
			ref:     "some-ref",
			wantErr: "must have an empty ref",
		},
		{
			name: "standard kind no ref ok",
			kind: StandardVTXOTimeoutExitPolicyKind,
		},
		{
			name:    "standard kind with ref errors",
			kind:    StandardVTXOTimeoutExitPolicyKind,
			ref:     "stale",
			wantErr: "must have an empty ref",
		},
		{
			name:    "custom kind without ref errors",
			kind:    ExitPolicyKind("vhtlc_claim"),
			wantErr: "requires a non-empty ref",
		},
		{
			name: "custom kind with ref ok",
			kind: ExitPolicyKind("vhtlc_claim"),
			ref:  "recovery-1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateExitPolicyIdentity(tc.kind, tc.ref)
			if tc.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestRegistryRefusesRestoreWhenResolverMissingKind verifies the boot-time
// admission gate: persisted non-terminal rows whose ExitPolicyKind is not
// covered by the configured resolver must surface as a startup error rather
// than being silently spawned and looping every block.
func TestRegistryRefusesRestoreWhenResolverMissingKind(t *testing.T) {
	target := wire.OutPoint{Hash: chainhash.Hash{0x33}, Index: 1}

	r := &registryBehavior{
		cfg: RegistryConfig{
			ExitSpendPolicyResolver: &fixedKindResolver{
				kinds: map[ExitPolicyKind]struct{}{
					StandardVTXOTimeoutExitPolicyKind: {},
				},
			},
		},
	}

	records := []RegistryRecord{{
		TargetOutpoint: target,
		ExitPolicyKind: "vhtlc_claim",
		ExitPolicyRef:  "ref-1",
	}}

	err := r.validateResolverCoversRecords(records)
	require.Error(t, err)
	require.Contains(t, err.Error(), "vhtlc_claim")
}

// TestRegistryRestoreAdmitsSupportedKinds verifies that records whose kinds
// are advertised by the resolver pass the boot-time admission gate.
func TestRegistryRestoreAdmitsSupportedKinds(t *testing.T) {
	r := &registryBehavior{
		cfg: RegistryConfig{
			ExitSpendPolicyResolver: &fixedKindResolver{
				kinds: map[ExitPolicyKind]struct{}{
					StandardVTXOTimeoutExitPolicyKind: {},
					"vhtlc_claim":                     {},
				},
			},
		},
	}

	records := []RegistryRecord{
		{
			TargetOutpoint: wire.OutPoint{
				Index: 1,
			},
			ExitPolicyKind: StandardVTXOTimeoutExitPolicyKind,
		},
		{
			TargetOutpoint: wire.OutPoint{
				Index: 2,
			},
			ExitPolicyKind: "vhtlc_claim",
			ExitPolicyRef:  "ref-1",
		},
		{
			// Empty kind normalises to standard; must pass.
			TargetOutpoint: wire.OutPoint{
				Index: 3,
			},
		},
	}

	require.NoError(t, r.validateResolverCoversRecords(records))
}

// TestRegistryRestoreSkipsCheckWhenResolverHasNoKindSupport documents the
// backwards-compatible default: a resolver that does not implement the
// optional ResolverKindSupport interface keeps the legacy behaviour and the
// admission gate is a no-op.
func TestRegistryRestoreSkipsCheckWhenResolverHasNoKindSupport(t *testing.T) {
	resolver := &recordingExitSpendPolicyResolver{}

	r := &registryBehavior{
		cfg: RegistryConfig{
			ExitSpendPolicyResolver: resolver,
		},
	}

	records := []RegistryRecord{{
		TargetOutpoint: wire.OutPoint{
			Index: 7,
		},
		ExitPolicyKind: "any-kind",
	}}

	require.NoError(t, r.validateResolverCoversRecords(records))
}

var _ RegistryStore = (*memRegistryStore)(nil)
var _ RegistryStore = (*flakyRegistryStore)(nil)
var _ RegistryStore = (*terminalFlakyRegistryStore)(nil)
var _ RegistryStore = (*alwaysFailUpsertRegistryStore)(nil)
var _ RegistryStore = (*blockingRegistryStore)(nil)
var _ actor.ActorRef[
	chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
] = (*fakeRegistryChainSourceRef)(nil)
