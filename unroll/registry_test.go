package unroll

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
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
func (s *memRegistryStore) GetRecord(_ context.Context,
	target wire.OutPoint) (*RegistryRecord, error) {

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
func (s *memRegistryStore) ListNonTerminalRecords(
	_ context.Context) ([]RegistryRecord, error) {

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
func (s *memRegistryStore) MarkTerminal(_ context.Context,
	target wire.OutPoint, phase Phase, failReason string,
	sweepTxid *chainhash.Hash) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	record := s.records[target]
	record.TargetOutpoint = target
	record.Phase = phase
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

// alwaysFailUpsertRegistryStore rejects every upsert while keeping read paths
// backed by the in-memory store.
type alwaysFailUpsertRegistryStore struct {
	*memRegistryStore
}

// newAlwaysFailUpsertRegistryStore creates a store that rejects every upsert.
func newAlwaysFailUpsertRegistryStore() *alwaysFailUpsertRegistryStore {
	return &alwaysFailUpsertRegistryStore{
		memRegistryStore: newMemRegistryStore(),
	}
}

// UpsertRecord always fails to simulate persistent control-plane contention.
func (s *alwaysFailUpsertRegistryStore) UpsertRecord(context.Context,
	RegistryRecord) error {

	return errors.New("injected upsert failure")
}

// blockingRegistryStore holds upserts until released so tests can verify that
// registry status remains available while persistence is stalled.
type blockingRegistryStore struct {
	*memRegistryStore

	started chan struct{}
	release chan struct{}
	once    sync.Once
}

// newBlockingRegistryStore creates a store with a gate around UpsertRecord.
func newBlockingRegistryStore() *blockingRegistryStore {
	return &blockingRegistryStore{
		memRegistryStore: newMemRegistryStore(),
		started:          make(chan struct{}),
		release:          make(chan struct{}),
	}
}

// UpsertRecord waits until the test releases the gate, then stores the record.
func (s *blockingRegistryStore) UpsertRecord(ctx context.Context,
	record RegistryRecord) error {

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
		promise.Complete(fn.Ok[chainsource.ChainSourceResp](
			&chainsource.BestHeightResponse{Height: f.height},
		))

	case *chainsource.FeeEstimateRequest:
		promise.Complete(fn.Ok[chainsource.ChainSourceResp](
			&chainsource.FeeEstimateResponse{SatPerVByte: 5},
		))

	case *chainsource.SubscribeBlocksRequest:
		promise.Complete(fn.Ok[chainsource.ChainSourceResp](
			&chainsource.SubscribeBlocksResponse{},
		))

	case *chainsource.RegisterSpendRequest:
		promise.Complete(fn.Ok[chainsource.ChainSourceResp](
			&chainsource.RegisterSpendResponse{},
		))

	default:
		promise.Complete(fn.Err[chainsource.ChainSourceResp](
			fmt.Errorf("unexpected chainsource msg %T", msg),
		))
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
		Store:          store,
		DeliveryStore:  checkpoints,
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   txconfirmRef,
		ChainSource:    &fakeRegistryChainSourceRef{height: 200},
		Wallet:         &fakeSweepWallet{},
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

	regBehavior.spawnFunc = func(_ context.Context,
		target wire.OutPoint) (*VTXOUnrollActor, error) {

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
		}
		childBehavior := &behavior{
			cfg: childCfg,
			log: btclog.Disabled,
		}
		err := childBehavior.restoreCheckpoint(t.Context())
		if err != nil {
			return nil, err
		}

		childActor := actor.NewActor(actor.ActorConfig[Msg, Resp]{
			ID:          actorIDForTarget(target),
			Behavior:    childBehavior,
			MailboxSize: 64,
		})
		childBehavior.selfRef = childActor.TellRef()
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
	txconfirmRef.setImmediateFailed(proof.RootTxids()[0], "rejected")

	_, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
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

// TestRegistryEnsureSurvivesInitialPersistFailure verifies that a transient
// control-plane upsert failure does not tear down the live child actor.
func TestRegistryEnsureSurvivesInitialPersistFailure(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
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

	resp, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.True(t, ensureResp.Created)

	require.Eventually(t, func() bool {
		resp, err := registry.Ref().Ask(
			t.Context(), &GetStatusRequest{
				Outpoint: proof.TargetOutpoint(),
			},
		).Await(t.Context()).Unpack()
		require.NoError(t, err)

		status, ok := resp.(*GetStatusResp)
		require.True(t, ok)

		return status.Found && status.Active &&
			status.Phase == PhaseMaterializing
	}, testTimeout, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		record, err := store.GetRecord(
			t.Context(), proof.TargetOutpoint(),
		)
		require.NoError(t, err)

		return record != nil && record.Phase == PhaseMaterializing
	}, testTimeout, 10*time.Millisecond)
}

// TestRegistryTerminalPersistFallbackAfterInitialFailure verifies that a child
// can still persist terminal state when the initial control-plane upsert
// failed.
func TestRegistryTerminalPersistFallbackAfterInitialFailure(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	store := newFlakyRegistryStore(1)
	checkpoints := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}
	txconfirmRef.setImmediateFailed(proof.RootTxids()[0], "rejected")

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
}

// TestRegistryStatusFallsBackToPendingTerminalRecord verifies that the
// registry can still answer status queries from memory after a child has
// terminated when every control-plane upsert fails.
func TestRegistryStatusFallsBackToPendingTerminalRecord(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	store := newAlwaysFailUpsertRegistryStore()
	checkpoints := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}
	txconfirmRef.setImmediateFailed(proof.RootTxids()[0], "rejected")

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

		record, err := store.GetRecord(
			t.Context(), proof.TargetOutpoint(),
		)
		require.NoError(t, err)

		return record == nil && status.ActorID != "" &&
			status.FailReason != ""
	}, testTimeout, 10*time.Millisecond)
}

// TestRegistryTerminalStatusRemainsQueryableWhilePersistBlocked verifies that
// the registry can still report a fast terminal failure while the control
// store is blocked on persisting the row.
func TestRegistryTerminalStatusRemainsQueryableWhilePersistBlocked(
	t *testing.T) {

	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	store := newBlockingRegistryStore()
	checkpoints := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}
	txconfirmRef.setImmediateFailed(proof.RootTxids()[0], "rejected")

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

var _ RegistryStore = (*memRegistryStore)(nil)
var _ RegistryStore = (*flakyRegistryStore)(nil)
var _ RegistryStore = (*alwaysFailUpsertRegistryStore)(nil)
var _ RegistryStore = (*blockingRegistryStore)(nil)
var _ actor.ActorRef[
	chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
] = (*fakeRegistryChainSourceRef)(nil)
