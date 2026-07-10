package darepod

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/stretchr/testify/require"
)

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
