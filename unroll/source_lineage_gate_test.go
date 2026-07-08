package unroll

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// stubBatchCanon is a minimal batchcanon.Store for the unroll source-lineage
// gate tests: it answers GetBatch from a txid->state map and returns zero
// values for the methods the gate never calls.
type stubBatchCanon struct {
	states map[chainhash.Hash]batchcanon.State
}

func (s *stubBatchCanon) GetBatch(_ context.Context, txid chainhash.Hash) (
	*batchcanon.Record, error) {

	st, ok := s.states[txid]
	if !ok {
		return nil, batchcanon.ErrBatchNotFound
	}

	return &batchcanon.Record{BatchTxID: txid, State: st}, nil
}

func (s *stubBatchCanon) UpsertBatch(context.Context,
	*batchcanon.Record) error {

	return nil
}

func (s *stubBatchCanon) ListBatchesByState(context.Context, batchcanon.State) (
	[]*batchcanon.Record, error) {

	return nil, nil
}

func (s *stubBatchCanon) UpdateBatchState(context.Context, chainhash.Hash,
	batchcanon.State) error {

	return nil
}

func (s *stubBatchCanon) RecordConfirmation(context.Context, chainhash.Hash,
	int32, chainhash.Hash) error {

	return nil
}

func (s *stubBatchCanon) ClearConfirmation(context.Context,
	chainhash.Hash) error {

	return nil
}

func (s *stubBatchCanon) FindBatchesConsumingOutpoint(context.Context,
	wire.OutPoint) ([]chainhash.Hash, error) {

	return nil, nil
}

func (s *stubBatchCanon) AddProvisionalConsumer(context.Context, wire.OutPoint,
	chainhash.Hash) error {

	return nil
}

func (s *stubBatchCanon) ListProvisionalConsumersForBatch(context.Context,
	chainhash.Hash) ([]wire.OutPoint, error) {

	return nil, nil
}

func (s *stubBatchCanon) DeleteProvisionalConsumersForBatch(context.Context,
	chainhash.Hash) error {

	return nil
}

var _ batchcanon.Store = (*stubBatchCanon)(nil)

// gateTarget builds a target outpoint + a descriptor whose lineage is the
// supplied commitment txids (first is the direct commitment, rest are
// cross-commitment ancestors).
func gateTarget(direct chainhash.Hash,
	ancestors ...chainhash.Hash) (wire.OutPoint, *vtxo.Descriptor) {

	op := wire.OutPoint{Hash: chainhash.Hash{0xfe}, Index: 0}
	desc := &vtxo.Descriptor{Outpoint: op, CommitmentTxID: direct}
	for _, a := range ancestors {
		desc.Ancestry = append(desc.Ancestry, vtxo.Ancestry{
			CommitmentTxID: a,
		})
	}

	return op, desc
}

func newGateBehavior(store batchcanon.Store,
	desc *vtxo.Descriptor) *registryBehavior {

	return &registryBehavior{
		cfg: RegistryConfig{
			BatchCanonicality: store,
			VTXOStore: &mockVTXOStore{
				desc: desc,
			},
		},
	}
}

// TestSourceLineageBlockedOnInvalidatedAncestor verifies a fresh unroll is
// refused when any batch in the target's lineage is permanently invalidated
// (conflict-finalized), even if its direct commitment is canonical.
func TestSourceLineageBlockedOnInvalidatedAncestor(t *testing.T) {
	t.Parallel()

	direct := chainhash.Hash{0xaa}
	ancestor := chainhash.Hash{0xbb}
	op, desc := gateTarget(direct, ancestor)

	b := newGateBehavior(&stubBatchCanon{
		states: map[chainhash.Hash]batchcanon.State{
			direct:   batchcanon.StateProvisional,
			ancestor: batchcanon.StateConflictFinalized,
		},
	}, desc)

	require.True(t, b.sourceLineageInvalidated(t.Context(), op))
}

// TestSourceLineagePermitsTransientReorg verifies a reorged-out (but not
// conflict-finalized) ancestor does NOT block a fresh unroll: the reorg is
// expected to self-heal, and blocking could drop a needed safety exit that
// cannot be retried.
func TestSourceLineagePermitsTransientReorg(t *testing.T) {
	t.Parallel()

	direct := chainhash.Hash{0xaa}
	ancestor := chainhash.Hash{0xbb}
	op, desc := gateTarget(direct, ancestor)

	b := newGateBehavior(&stubBatchCanon{
		states: map[chainhash.Hash]batchcanon.State{
			direct:   batchcanon.StateProvisional,
			ancestor: batchcanon.StateReorgedOut,
		},
	}, desc)

	require.False(t, b.sourceLineageInvalidated(t.Context(), op))
}

// TestSourceLineageNotBlockedWhenCanonical verifies a fresh unroll is admitted
// when the whole lineage is canonical.
func TestSourceLineageNotBlockedWhenCanonical(t *testing.T) {
	t.Parallel()

	direct := chainhash.Hash{0xaa}
	ancestor := chainhash.Hash{0xbb}
	op, desc := gateTarget(direct, ancestor)

	b := newGateBehavior(&stubBatchCanon{
		states: map[chainhash.Hash]batchcanon.State{
			direct:   batchcanon.StateFinalized,
			ancestor: batchcanon.StateProvisional,
		},
	}, desc)

	require.False(t, b.sourceLineageInvalidated(t.Context(), op))
}

// TestSourceLineagePermissiveWhenUnregistered verifies the gate does not block
// when the lineage batches are not registered (unseen), preserving the
// permissive default.
func TestSourceLineagePermissiveWhenUnregistered(t *testing.T) {
	t.Parallel()

	op, desc := gateTarget(chainhash.Hash{0xaa}, chainhash.Hash{0xbb})

	b := newGateBehavior(&stubBatchCanon{
		states: map[chainhash.Hash]batchcanon.State{},
	}, desc)

	require.False(t, b.sourceLineageInvalidated(t.Context(), op))
}

// TestSourceLineagePermissiveWhenVTXOLoadFails verifies the gate fails
// permissive (admits) when the target descriptor cannot be loaded, rather than
// blocking an exit on a transient store error.
func TestSourceLineagePermissiveWhenVTXOLoadFails(t *testing.T) {
	t.Parallel()

	op, _ := gateTarget(chainhash.Hash{0xaa})

	b := &registryBehavior{
		cfg: RegistryConfig{
			BatchCanonicality: &stubBatchCanon{
				states: map[chainhash.Hash]batchcanon.State{},
			},
			VTXOStore: &mockVTXOStore{
				err: errors.New("boom"),
			},
		},
		log: btclog.Disabled,
	}

	require.False(t, b.sourceLineageInvalidated(t.Context(), op))
}

// TestSourceLineageGateDormantWhenNoStore verifies the gate is a no-op when no
// canonicality store is wired.
func TestSourceLineageGateDormantWhenNoStore(t *testing.T) {
	t.Parallel()

	op, _ := gateTarget(chainhash.Hash{0xaa})

	b := &registryBehavior{cfg: RegistryConfig{}}

	require.False(t, b.sourceLineageInvalidated(t.Context(), op))
}

// TestErrSourceLineageUnavailableIsSentinel guards that the wrapped form
// handleEnsure returns is matchable via errors.Is, so RPC/chain-resolver
// callers can classify a lineage-refused unroll.
func TestErrSourceLineageUnavailableIsSentinel(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("%w: %s", ErrSourceLineageUnavailable,
		"some-outpoint")
	require.ErrorIs(t, wrapped, ErrSourceLineageUnavailable)
}
