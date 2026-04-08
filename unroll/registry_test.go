package unroll

import (
	"context"
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

// cloneRegistryRecord deep-copies one registry record.
func cloneRegistryRecord(record RegistryRecord) RegistryRecord {
	record.SweepTxid = copyHash(record.SweepTxid)
	return record
}

// fakeRegistryChainSourceRef is a minimal chainsource actor ref for registry
// tests.
type fakeRegistryChainSourceRef struct {
	mu     sync.Mutex
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
	msg chainsource.ChainSourceMsg) actor.Future[chainsource.ChainSourceResp] {

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
		cfg:    cfg,
		log:    btclog.Disabled,
		active: make(map[wire.OutPoint]*VTXOUnrollActor),
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

		childBehavior := &behavior{
			cfg: Config{
				TargetOutpoint:             target,
				ActorID:                    actorIDForTarget(target),
				DeliveryStore:              cfg.DeliveryStore,
				ProofAssembler:             cfg.ProofAssembler,
				VTXOStore:                  cfg.VTXOStore,
				TxConfirmRef:               cfg.TxConfirmRef,
				ChainSource:                cfg.ChainSource,
				Wallet:                     cfg.Wallet,
				MaxSweepFeeRateSatPerVByte: cfg.MaxSweepFeeRateSatPerVByte,
				RegistryRef:                registryActor.TellRef(),
			},
			log: btclog.Disabled,
		}
		err := childBehavior.restoreCheckpoint(context.Background())
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

	resp, err := registry.Ref().Ask(context.Background(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(context.Background()).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.True(t, ensureResp.Created)
	require.Equal(t, 1, txconfirmRef.requestCount())

	resp, err = registry.Ref().Ask(context.Background(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(context.Background()).Unpack()
	require.NoError(t, err)

	ensureResp, ok = resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.False(t, ensureResp.Created)
	require.Equal(t, 1, txconfirmRef.requestCount())

	record, err := store.GetRecord(context.Background(), proof.TargetOutpoint())
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

	_, err := registry.Ref().Ask(context.Background(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(context.Background()).Unpack()
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		resp, err := registry.Ref().Ask(
			context.Background(), &GetStatusRequest{
				Outpoint: proof.TargetOutpoint(),
			},
		).Await(context.Background()).Unpack()
		require.NoError(t, err)

		status, ok := resp.(*GetStatusResp)
		require.True(t, ok)
		return status.Found && !status.Active && status.Phase == PhaseFailed
	}, testTimeout, 10*time.Millisecond)

	record, err := store.GetRecord(context.Background(), proof.TargetOutpoint())
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, PhaseFailed, record.Phase)
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
	err = checkpoints.SaveCheckpoint(context.Background(), actor.CheckpointParams{
		ActorID:   actorID,
		StateType: checkpointStateType,
		StateData: raw,
		Version:   checkpointVersion,
	})
	require.NoError(t, err)

	err = store.UpsertRecord(context.Background(), RegistryRecord{
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

	err = registry.RestoreNonTerminal(context.Background())
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(proof.RootTxids()[0]) == 1
	}, testTimeout, 10*time.Millisecond)

	resp, err := registry.Ref().Ask(context.Background(), &GetStatusRequest{
		Outpoint: proof.TargetOutpoint(),
	}).Await(context.Background()).Unpack()
	require.NoError(t, err)

	status, ok := resp.(*GetStatusResp)
	require.True(t, ok)
	require.True(t, status.Found)
	require.True(t, status.Active)
	require.Equal(t, PhaseMaterializing, status.Phase)
}

var _ RegistryStore = (*memRegistryStore)(nil)
var _ actor.ActorRef[
	chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
] = (*fakeRegistryChainSourceRef)(nil)
