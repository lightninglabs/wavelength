package vtxo

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// fakeBatchCanon is a minimal batchcanon.Store for the lineage-gate tests: it
// maps batch txids to canonicality states and answers GetBatch from that map.
// The other Store methods are unused by the gate and return zero values.
type fakeBatchCanon struct {
	states map[chainhash.Hash]batchcanon.State
}

func (f *fakeBatchCanon) GetBatch(_ context.Context, txid chainhash.Hash) (
	*batchcanon.Record, error) {

	st, ok := f.states[txid]
	if !ok {
		return nil, batchcanon.ErrBatchNotFound
	}

	return &batchcanon.Record{BatchTxID: txid, State: st}, nil
}

func (f *fakeBatchCanon) UpsertBatch(context.Context,
	*batchcanon.Record) error {

	return nil
}

func (f *fakeBatchCanon) ListBatchesByState(context.Context, batchcanon.State) (
	[]*batchcanon.Record, error) {

	return nil, nil
}

func (f *fakeBatchCanon) UpdateBatchState(context.Context, chainhash.Hash,
	batchcanon.State) error {

	return nil
}

func (f *fakeBatchCanon) RecordConfirmation(context.Context, chainhash.Hash,
	int32, chainhash.Hash) error {

	return nil
}

func (f *fakeBatchCanon) RecordInputConflict(context.Context, chainhash.Hash,
	wire.OutPoint, bool, bool) error {

	return nil
}

func (f *fakeBatchCanon) ClearConfirmation(context.Context,
	chainhash.Hash) error {

	return nil
}

func (f *fakeBatchCanon) FindBatchesConsumingOutpoint(context.Context,
	wire.OutPoint) ([]chainhash.Hash, error) {

	return nil, nil
}

func (f *fakeBatchCanon) AddProvisionalConsumer(context.Context, wire.OutPoint,
	chainhash.Hash) error {

	return nil
}

func (f *fakeBatchCanon) ListProvisionalConsumersForBatch(context.Context,
	chainhash.Hash) ([]wire.OutPoint, error) {

	return nil, nil
}

func (f *fakeBatchCanon) DeleteProvisionalConsumersForBatch(context.Context,
	chainhash.Hash) error {

	return nil
}

var _ batchcanon.Store = (*fakeBatchCanon)(nil)

// TestSelectExcludesLimboLineage verifies the admission gate drops a candidate
// whose batch reorged out (limbo), so largest-first selection skips it and
// picks a smaller candidate whose batch is canonical instead.
func TestSelectExcludesLimboLineage(t *testing.T) {
	t.Parallel()

	good := makeDescriptor(t, 40000, 0)
	bad := makeDescriptor(t, 50000, 1)
	good.CommitmentTxID = chainhash.Hash{0xaa}
	bad.CommitmentTxID = chainhash.Hash{0xbb}

	mgr, store := newTestManager(t, []*Descriptor{good, bad})
	mgr.cfg.BatchCanonicality = &fakeBatchCanon{
		states: map[chainhash.Hash]batchcanon.State{
			good.CommitmentTxID: batchcanon.StateProvisional,
			bad.CommitmentTxID:  batchcanon.StateReorgedOut,
		},
	}

	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{good, bad}, nil)
	store.On("GetVTXO", mock.Anything, good.Outpoint).Return(good, nil)
	store.On("GetVTXO", mock.Anything, bad.Outpoint).Return(bad, nil)

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount: 40000,
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	spendResp, ok := resp.(*SelectAndReserveSpendResponse)
	require.True(t, ok)

	// The 50000 candidate (largest) is gated out for its reorged-out batch,
	// so selection falls to the 40000 candidate with a canonical batch.
	require.Len(t, spendResp.SelectedVTXOs, 1)
	require.Equal(t, good.Outpoint, spendResp.SelectedVTXOs[0].Outpoint)
}

// TestSelectFailsWhenAllLineageInvalidated verifies that when every candidate's
// batch is invalidated, selection finds no admissible liquidity and fails
// rather than spending an invalidated VTXO.
func TestSelectFailsWhenAllLineageInvalidated(t *testing.T) {
	t.Parallel()

	only := makeDescriptor(t, 50000, 0)
	only.CommitmentTxID = chainhash.Hash{0xcc}

	mgr, store := newTestManager(t, []*Descriptor{only})
	mgr.cfg.BatchCanonicality = &fakeBatchCanon{
		states: map[chainhash.Hash]batchcanon.State{
			only.CommitmentTxID: batchcanon.StateConflictFinalized,
		},
	}

	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{only}, nil)
	store.On("GetVTXO", mock.Anything, only.Outpoint).Return(only, nil)

	// The shortfall path builds a liquidity diagnostic via ListLiveVTXOs.
	store.On("ListLiveVTXOs", mock.Anything).Return(
		[]*Descriptor{only}, nil,
	)

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount: 40000,
	})
	_, err := result.Unpack()
	require.Error(t, err)
}

// TestSelectAdmitsCanonicalAndUnregisteredLineage verifies the gate is
// permissive: a candidate whose batch is provisional is admitted, and so is
// one whose batch has no canonicality record yet (unregistered during
// rollout) — only positively limbo/invalidated lineage is refused.
func TestSelectAdmitsCanonicalAndUnregisteredLineage(t *testing.T) {
	t.Parallel()

	provisional := makeDescriptor(t, 30000, 0)
	unregistered := makeDescriptor(t, 50000, 1)
	provisional.CommitmentTxID = chainhash.Hash{0xd1}
	unregistered.CommitmentTxID = chainhash.Hash{0xd2}

	mgr, store := newTestManager(t, []*Descriptor{
		provisional, unregistered,
	})
	mgr.cfg.BatchCanonicality = &fakeBatchCanon{
		states: map[chainhash.Hash]batchcanon.State{
			provisional.CommitmentTxID: batchcanon.StateProvisional,
			// unregistered: intentionally absent from the map.
		},
	}

	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{provisional, unregistered}, nil)
	store.On(
		"GetVTXO", mock.Anything, provisional.Outpoint,
	).Return(provisional, nil)
	store.On(
		"GetVTXO", mock.Anything, unregistered.Outpoint,
	).Return(unregistered, nil)

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount: 40000,
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	spendResp, ok := resp.(*SelectAndReserveSpendResponse)
	require.True(t, ok)

	// The unregistered-batch candidate (50000, largest) is admitted because
	// the gate does not block unseen/unregistered lineage.
	require.Len(t, spendResp.SelectedVTXOs, 1)
	require.Equal(
		t, unregistered.Outpoint, spendResp.SelectedVTXOs[0].Outpoint,
	)
}

// TestSelectGateDisabledWhenNoStore verifies that with no canonicality store
// configured the gate is a complete no-op (no GetVTXO calls, normal
// largest-first selection).
func TestSelectGateDisabledWhenNoStore(t *testing.T) {
	t.Parallel()

	v := makeDescriptor(t, 50000, 0)
	mgr, store := newTestManager(t, []*Descriptor{v})
	require.Nil(t, mgr.cfg.BatchCanonicality)

	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{v}, nil)

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount: 40000,
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	spendResp, ok := resp.(*SelectAndReserveSpendResponse)
	require.True(t, ok)
	require.Len(t, spendResp.SelectedVTXOs, 1)
	require.Equal(t, v.Outpoint, spendResp.SelectedVTXOs[0].Outpoint)

	// The gate must not have queried GetVTXO at all.
	store.AssertNotCalled(t, "GetVTXO", mock.Anything, mock.Anything)
}
