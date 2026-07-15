package unrollplan

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestSweepStatusString locks in the stable debug labels.
func TestSweepStatusString(t *testing.T) {
	require.Equal(t, "pending", SweepStatusPending.String())
	require.Equal(t, "broadcasted", SweepStatusBroadcasted.String())
	require.Equal(t, "confirmed", SweepStatusConfirmed.String())
	require.Contains(t, SweepStatus(99).String(), "unknown")
}

// TestPlannerProofAccessor verifies the receiver-nil path and the happy path.
func TestPlannerProofAccessor(t *testing.T) {
	var nilPlanner *Planner
	require.Nil(t, nilPlanner.Proof())

	planner, proof := newPlannerFixture(t, linearProofFixture(t))
	require.Same(t, proof, planner.Proof())
}

// TestValidateSweepConfirmedRejectsEarlyHeight exercises the M-5 CSV-maturity
// invariant: a persisted state cannot claim the sweep confirmed before the
// target's csv delay has elapsed.
func TestValidateSweepConfirmedRejectsEarlyHeight(t *testing.T) {
	planner, proof := newPlannerFixture(t, linearProofFixture(t))

	confirmHeight := int32(100)
	sweepTxid := hashFromLabel("sweep-early")
	sweepHeight := int32(101) // CSV is 5, so 105 is the earliest valid.
	_, err := planner.Plan(110, &State{
		ConfirmedTxids: []chainhash.Hash{
			proof.Layers()[0][0],
			proof.TargetOutpoint().Hash,
		},
		TargetConfirmHeight: fn.Some(confirmHeight),
		Sweep: SweepState{
			Status:        SweepStatusConfirmed,
			Txid:          fn.Some(sweepTxid),
			ConfirmHeight: fn.Some(sweepHeight),
		},
	})
	require.ErrorContains(t, err, "before csv maturity")
}

// TestValidateSweepTxidCollision verifies the M-7 guard: the sweep txid
// cannot match any proof node's txid.
func TestValidateSweepTxidCollision(t *testing.T) {
	planner, proof := newPlannerFixture(t, linearProofFixture(t))

	// Use an existing proof node's txid as the sweep txid.
	collision := proof.Layers()[0][0]

	_, err := planner.Plan(100, &State{
		ConfirmedTxids: []chainhash.Hash{
			proof.Layers()[0][0],
			proof.TargetOutpoint().Hash,
		},
		TargetConfirmHeight: fn.Some(int32(100)),
		Sweep: SweepState{
			Status: SweepStatusBroadcasted,
			Txid:   fn.Some(collision),
		},
	})
	require.ErrorContains(t, err, "collides with proof node")
}

// TestValidateSweepBroadcastedRequiresConfirmedTarget exercises H-5.
func TestValidateSweepBroadcastedRequiresConfirmedTarget(t *testing.T) {
	planner, _ := newPlannerFixture(t, linearProofFixture(t))

	sweep := hashFromLabel("sweep-no-target")
	_, err := planner.Plan(100, &State{
		Sweep: SweepState{
			Status: SweepStatusBroadcasted,
			Txid:   fn.Some(sweep),
		},
	})
	require.ErrorContains(
		t, err, "broadcasted sweep requires confirmed target",
	)
}

// TestValidateSweepNegativeHeightRejected covers the negative-height guard
// in validateSweepState for confirmed sweeps.
func TestValidateSweepNegativeHeightRejected(t *testing.T) {
	planner, proof := newPlannerFixture(t, linearProofFixture(t))

	sweep := hashFromLabel("sweep-neg")
	_, err := planner.Plan(100, &State{
		ConfirmedTxids: []chainhash.Hash{
			proof.Layers()[0][0],
			proof.TargetOutpoint().Hash,
		},
		TargetConfirmHeight: fn.Some(int32(100)),
		Sweep: SweepState{
			Status:        SweepStatusConfirmed,
			Txid:          fn.Some(sweep),
			ConfirmHeight: fn.Some(int32(-1)),
		},
	})
	require.ErrorContains(t, err, "negative")
}

// TestValidateSweepPendingWithTxidRejected exercises the SweepStatusPending
// arm of validateSweepState, which forbids a txid or confirm height on a
// pending sweep.
func TestValidateSweepPendingWithTxidRejected(t *testing.T) {
	planner, _ := newPlannerFixture(t, linearProofFixture(t))

	_, err := planner.Plan(100, &State{
		Sweep: SweepState{
			Status: SweepStatusPending,
			Txid:   fn.Some(hashFromLabel("sweep-pending")),
		},
	})
	require.ErrorContains(t, err, "pending sweep must not have a txid")
}

// TestStateValidateNegativeTargetHeight exercises the C-2 guard on
// TargetConfirmHeight.
func TestStateValidateNegativeTargetHeight(t *testing.T) {
	_, proof := newPlannerFixture(t, linearProofFixture(t))
	state := &State{
		ConfirmedTxids: []chainhash.Hash{
			proof.Layers()[0][0],
			proof.TargetOutpoint().Hash,
		},
		TargetConfirmHeight: fn.Some(int32(-1)),
	}
	require.ErrorContains(t, state.Validate(proof),
		"target confirm height")
}

// TestCSVInfoRejectsMissingTargetHeight ensures the planner surfaces a clear
// error when the target is confirmed but a caller crafts the state without a
// target confirm height.
func TestCSVInfoRejectsMissingTargetHeight(t *testing.T) {
	_, proof := newPlannerFixture(t, linearProofFixture(t))
	_, err := csvInfoAt(proof.CSVDelay(), 100, fn.None[int32]())
	require.Error(t, err)
}

// TestSortBlockedDeterminism exercises the sortBlocked helper directly so its
// per-layer and per-txid ordering rules are covered even when Plan never
// produces enough blocked entries to trigger both code paths.
func TestSortBlockedDeterminism(t *testing.T) {
	hashA := hashFromByte(1)
	hashB := hashFromByte(2)
	hashC := hashFromByte(3)

	_ = chainhash.Hash{} // keep import used

	tb := []BlockedTx{
		{
			TxFrontier: TxFrontier{
				Txid:  hashC,
				Layer: 2,
			},
		},
		{
			TxFrontier: TxFrontier{
				Txid:  hashA,
				Layer: 1,
			},
		},
		{
			TxFrontier: TxFrontier{
				Txid:  hashB,
				Layer: 1,
			},
		},
	}

	sortBlocked(tb)
	require.Equal(t, hashA, tb[0].Txid)
	require.Equal(t, hashB, tb[1].Txid)
	require.Equal(t, hashC, tb[2].Txid)
}

// TestProofAccessor verifies the Proof accessor returns the passed-in proof
// so callers can retrieve the immutable graph without a separate ref.
func TestProofAccessor(t *testing.T) {
	planner, proof := newPlannerFixture(t, linearProofFixture(t))
	require.IsType(t, &recovery.Proof{}, proof)
	require.Same(t, proof, planner.Proof())
}
