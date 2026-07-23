package waved

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// balanceCanonReader is a focused canonicality reader for balance tests.
type balanceCanonReader struct {
	records map[chainhash.Hash]*batchcanon.Record
	err     error
}

// GetBatch returns the seeded record or canonical not-found result.
func (r *balanceCanonReader) GetBatch(_ context.Context, txid chainhash.Hash) (
	*batchcanon.Record, error) {

	if r.err != nil {
		return nil, r.err
	}
	record, ok := r.records[txid]
	if !ok {
		return nil, batchcanon.ErrBatchNotFound
	}

	return record, nil
}

// readyBalanceRecord builds the minimum ready record required by an
// availability query. Its immutable transaction bytes are irrelevant to this
// read-only test; registration validation covers their bindings separately.
func readyBalanceRecord(txid chainhash.Hash,
	state batchcanon.State) *batchcanon.Record {

	return &batchcanon.Record{
		BatchTxID: txid,
		BatchTx: []byte{
			1,
		},
		RegistrationStage:     batchcanon.RegistrationComplete,
		ObservationGeneration: 1,
		ReadyGeneration:       fn.Some(uint64(1)),
		State:                 state,
		ConfirmationPkScript: []byte{
			1,
		},
		ConsumedInputs: []batchcanon.ConsumedInput{{
			PkScript: []byte{
				1,
			},
		}},
	}
}

// balanceDescriptor builds a lifecycle-live VTXO under one batch.
func balanceDescriptor(i byte, amount btcutil.Amount,
	batch chainhash.Hash) *vtxo.Descriptor {

	return &vtxo.Descriptor{
		Outpoint: wire.OutPoint{
			Hash:  chainhash.HashH([]byte{i}),
			Index: uint32(i),
		},
		Amount:         amount,
		CommitmentTxID: batch,
		Status:         vtxo.VTXOStatusLive,
	}
}

// TestCanonicalityAwareVTXOBalance verifies the user-visible balance excludes
// unusable lineage while preserving reversible value in the temporary bucket.
func TestCanonicalityAwareVTXOBalance(t *testing.T) {
	t.Parallel()

	batch := func(label string) chainhash.Hash {
		return chainhash.HashH([]byte(label))
	}
	finalBatch := batch("final")
	provisionalBatch := batch("provisional")
	reorgBatch := batch("reorg")
	conflictBatch := batch("conflict")
	unseenBatch := batch("unseen")
	reconcilingBatch := batch("reconciling")
	invalidBatch := batch("invalid")

	reader := &balanceCanonReader{
		records: map[chainhash.Hash]*batchcanon.Record{
			finalBatch: readyBalanceRecord(
				finalBatch, batchcanon.StateFinalized,
			),
			provisionalBatch: readyBalanceRecord(
				provisionalBatch, batchcanon.StateProvisional,
			),
			reorgBatch: readyBalanceRecord(
				reorgBatch, batchcanon.StateReorgedOut,
			),
			conflictBatch: readyBalanceRecord(
				conflictBatch,
				batchcanon.StateConflictProvisional,
			),
			unseenBatch: readyBalanceRecord(
				unseenBatch, batchcanon.StateUnseen,
			),
			reconcilingBatch: {
				BatchTxID: reconcilingBatch,
			},
			invalidBatch: readyBalanceRecord(
				invalidBatch, batchcanon.StateConflictFinalized,
			),
		},
	}

	descs := []*vtxo.Descriptor{
		balanceDescriptor(1, 10_000, finalBatch),
		balanceDescriptor(2, 20_000, provisionalBatch),
		balanceDescriptor(3, 30_000, reorgBatch),
		balanceDescriptor(4, 40_000, conflictBatch),
		balanceDescriptor(5, 50_000, unseenBatch),
		balanceDescriptor(6, 60_000, reconcilingBatch),
		balanceDescriptor(7, 70_000, invalidBatch),
	}

	spendable, unavailable, err := vtxo.ClassifyCanonicalityBalance(
		t.Context(), descs, reader,
	)
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(30_000), spendable)
	require.Equal(t, btcutil.Amount(180_000), unavailable)
}

// TestCanonicalityAwareVTXOBalanceUsesWorstParent verifies that any unusable
// parent moves the full multi-parent VTXO into the temporary bucket.
func TestCanonicalityAwareVTXOBalanceUsesWorstParent(t *testing.T) {
	t.Parallel()

	finalBatch := chainhash.HashH([]byte("final"))
	reorgBatch := chainhash.HashH([]byte("reorg"))
	reader := &balanceCanonReader{
		records: map[chainhash.Hash]*batchcanon.Record{
			finalBatch: readyBalanceRecord(
				finalBatch, batchcanon.StateFinalized,
			),
			reorgBatch: readyBalanceRecord(
				reorgBatch, batchcanon.StateReorgedOut,
			),
		},
	}
	desc := balanceDescriptor(1, 25_000, finalBatch)
	desc.Ancestry = []vtxo.Ancestry{{
		CommitmentTxID: reorgBatch,
	}}

	spendable, unavailable, err := vtxo.ClassifyCanonicalityBalance(
		t.Context(), []*vtxo.Descriptor{desc}, reader,
	)
	require.NoError(t, err)
	require.Zero(t, spendable)
	require.Equal(t, btcutil.Amount(25_000), unavailable)
}

// TestCanonicalityAwareVTXOBalanceLegacy preserves the behavior-neutral path
// used while the canonicality gate is disabled.
func TestCanonicalityAwareVTXOBalanceLegacy(t *testing.T) {
	t.Parallel()

	desc := balanceDescriptor(
		1, 25_000,
		chainhash.HashH(
			[]byte("batch"),
		),
	)
	spendable, unavailable, err := vtxo.ClassifyCanonicalityBalance(
		t.Context(), []*vtxo.Descriptor{desc}, nil,
	)
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(25_000), spendable)
	require.Zero(t, unavailable)
}

// TestCanonicalityAwareVTXOBalancePropagatesReaderError ensures a broken
// authority cannot be presented to the user as a zero or spendable balance.
func TestCanonicalityAwareVTXOBalancePropagatesReaderError(t *testing.T) {
	t.Parallel()

	desc := balanceDescriptor(
		1, 25_000,
		chainhash.HashH(
			[]byte("batch"),
		),
	)
	_, _, err := vtxo.ClassifyCanonicalityBalance(
		t.Context(), []*vtxo.Descriptor{desc}, &balanceCanonReader{
			err: errors.New("reader unavailable"),
		},
	)
	require.ErrorContains(t, err, "reader unavailable")
}

// fakeExitJobLookup is an in-memory exitJobLookup. A missing outpoint returns
// db.ErrUnilateralExitJobNotFound, mirroring the real store.
type fakeExitJobLookup struct {
	jobs map[wire.OutPoint]*db.UnilateralExitJobRecord
	err  error
}

// GetJob returns the seeded job for an outpoint, the seeded error, or
// db.ErrUnilateralExitJobNotFound.
func (f *fakeExitJobLookup) GetJob(_ context.Context, target wire.OutPoint) (
	*db.UnilateralExitJobRecord, error) {

	if f.err != nil {
		return nil, f.err
	}

	job, ok := f.jobs[target]
	if !ok {
		return nil, db.ErrUnilateralExitJobNotFound
	}

	return job, nil
}

// exitOutpoint builds a distinct outpoint from an index.
func exitOutpoint(i byte) wire.OutPoint {
	var hash chainhash.Hash
	hash[0] = i

	return wire.OutPoint{Hash: hash, Index: uint32(i)}
}

// TestSumWalletUnilateralExits checks that wallet exits (standard policy or
// no job) are summed and recovery-only targets (non-standard policy) skipped.
func TestSumWalletUnilateralExits(t *testing.T) {
	t.Parallel()

	var (
		standardOp = exitOutpoint(1)
		emptyOp    = exitOutpoint(2)
		noJobOp    = exitOutpoint(3)
		claimOp    = exitOutpoint(4)
		refundOp   = exitOutpoint(5)
	)

	exiting := []*vtxo.Descriptor{
		{
			Outpoint: standardOp,
			Amount:   10_000,
		},
		{
			Outpoint: emptyOp,
			Amount:   20_000,
		},
		{
			Outpoint: noJobOp,
			Amount:   40_000,
		},
		{
			Outpoint: claimOp,
			Amount:   80_000,
		},
		{
			Outpoint: refundOp,
			Amount:   160_000,
		},
	}

	// The two vHTLC recovery policy kinds, aliased to keep the map short.
	claimKind := string(actormsg.ExitPolicyVHTLCClaim)
	refundKind := string(actormsg.ExitPolicyVHTLCRefundWithoutReceiver)

	job := func(op wire.OutPoint, kind string) *db.UnilateralExitJobRecord {
		return &db.UnilateralExitJobRecord{
			TargetOutpoint: op,
			ExitPolicyKind: kind,
		}
	}

	// noJobOp intentionally has no entry: a VTXO with no exit job.
	lookup := &fakeExitJobLookup{
		jobs: map[wire.OutPoint]*db.UnilateralExitJobRecord{
			standardOp: job(standardOp, "standard_vtxo_timeout"),
			emptyOp:    job(emptyOp, ""),
			claimOp:    job(claimOp, claimKind),
			refundOp:   job(refundOp, refundKind),
		},
	}

	// Wallet exits only: 10_000 + 20_000 + 40_000.
	total, err := sumWalletUnilateralExits(t.Context(), exiting, lookup)
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(70_000), total)
}

// TestSumWalletUnilateralExitsPropagatesError confirms a store failure is
// surfaced rather than counted as zero.
func TestSumWalletUnilateralExitsPropagatesError(t *testing.T) {
	t.Parallel()

	lookup := &fakeExitJobLookup{err: errors.New("store down")}
	exiting := []*vtxo.Descriptor{
		{
			Outpoint: exitOutpoint(1),
			Amount:   10_000,
		},
	}

	_, err := sumWalletUnilateralExits(t.Context(), exiting, lookup)
	require.Error(t, err)
}

// TestSumWalletUnilateralExitsEmpty guards the empty-input case.
func TestSumWalletUnilateralExitsEmpty(t *testing.T) {
	t.Parallel()

	lookup := &fakeExitJobLookup{}
	total, err := sumWalletUnilateralExits(t.Context(), nil, lookup)
	require.NoError(t, err)
	require.Zero(t, total)
}
