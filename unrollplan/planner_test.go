package unrollplan

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

func TestPlanLinearProof(t *testing.T) {
	planner, proof := newPlannerFixture(t,
		linearProofFixture(t),
	)

	snapshot, err := planner.Plan(100, &State{})
	require.NoError(t, err)

	require.Len(t, snapshot.Ready, 1)
	require.Equal(t, proof.RootTxids()[0], snapshot.Ready[0].Txid)
	require.Len(t, snapshot.Blocked, 1)
	require.Equal(t, proof.TargetOutpoint().Hash, snapshot.Blocked[0].Txid)
	require.False(t, snapshot.TargetConfirmed)
	require.False(t, snapshot.AllProofConfirmed)
	require.False(t, snapshot.NeedSweep)
	require.False(t, snapshot.Done)
}

func TestPlanMultiParentProof(t *testing.T) {
	planner, proof := newPlannerFixture(t,
		multiParentProofFixture(t),
	)

	snapshot, err := planner.Plan(100, &State{})
	require.NoError(t, err)

	require.Len(t, snapshot.Ready, 2)
	require.Equal(t, proof.Layers()[0][0], snapshot.Ready[0].Txid)
	require.Equal(t, proof.Layers()[0][1], snapshot.Ready[1].Txid)
	require.Len(t, snapshot.Blocked, 1)
	require.ElementsMatch(t,
		[]chainhash.Hash{
			proof.Layers()[0][0],
			proof.Layers()[0][1],
		},
		snapshot.Blocked[0].MissingParents,
	)
	require.False(t, snapshot.AllProofConfirmed)
}

func TestPlanPartialConfirmation(t *testing.T) {
	planner, proof := newPlannerFixture(t,
		threeLayerProofFixture(t),
	)

	root := proof.Layers()[0][0]
	state := &State{
		ConfirmedTxids: []chainhash.Hash{
			root,
		},
	}

	snapshot, err := planner.Plan(100, state)
	require.NoError(t, err)

	require.Len(t, snapshot.Ready, 1)
	require.Equal(t, proof.Layers()[1][0], snapshot.Ready[0].Txid)
	require.Len(t, snapshot.InFlight, 0)
	require.Len(t, snapshot.Blocked, 1)
	require.Equal(t, proof.TargetOutpoint().Hash, snapshot.Blocked[0].Txid)
	require.Equal(
		t, []chainhash.Hash{proof.Layers()[1][0]},
		snapshot.Blocked[0].MissingParents,
	)
	require.False(t, snapshot.AllProofConfirmed)
}

func TestPlanTargetConfirmedCSVPending(t *testing.T) {
	planner, proof := newPlannerFixture(t,
		linearProofFixture(t),
	)

	confirmHeight := int32(100)
	state := &State{
		ConfirmedTxids: []chainhash.Hash{
			proof.Layers()[0][0],
			proof.TargetOutpoint().Hash,
		},
		TargetConfirmHeight: fn.Some(confirmHeight),
	}

	snapshot, err := planner.Plan(102, state)
	require.NoError(t, err)

	require.True(t, snapshot.TargetConfirmed)
	require.True(t, snapshot.AllProofConfirmed)
	require.Equal(
		t, confirmHeight, snapshot.TargetConfirmHeight.UnwrapOrFail(t),
	)
	csv := snapshot.CSV.UnwrapOrFail(t)
	require.False(t, csv.Ready)
	require.Equal(t, int32(3), csv.BlocksRemaining)
	require.False(t, snapshot.NeedSweep)
	require.False(t, snapshot.Done)
}

// TestPlanCooperativeSpendSkipsProofCSV verifies a custom final spend can use
// the confirmed VTXO immediately without weakening the proof's timeout path.
func TestPlanCooperativeSpendSkipsProofCSV(t *testing.T) {
	proof := linearProofFixture(t)
	planner, err := NewPlannerWithSweepCSVDelay(proof, 0)
	require.NoError(t, err)

	confirmHeight := int32(100)
	state := &State{
		ConfirmedTxids: []chainhash.Hash{
			proof.Layers()[0][0],
			proof.TargetOutpoint().Hash,
		},
		TargetConfirmHeight: fn.Some(confirmHeight),
	}

	snapshot, err := planner.Plan(confirmHeight, state)
	require.NoError(t, err)
	require.True(t, snapshot.CSV.UnwrapOrFail(t).Ready)
	require.Zero(t, snapshot.CSV.UnwrapOrFail(t).BlocksRemaining)
	require.True(t, snapshot.NeedSweep)
}

func TestPlanCSVReadyNeedsSweep(t *testing.T) {
	planner, proof := newPlannerFixture(t,
		linearProofFixture(t),
	)

	confirmHeight := int32(100)
	state := &State{
		ConfirmedTxids: []chainhash.Hash{
			proof.Layers()[0][0],
			proof.TargetOutpoint().Hash,
		},
		TargetConfirmHeight: fn.Some(confirmHeight),
	}

	snapshot, err := planner.Plan(105, state)
	require.NoError(t, err)

	require.True(t, snapshot.TargetConfirmed)
	require.True(t, snapshot.CSV.UnwrapOrFail(t).Ready)
	require.True(t, snapshot.NeedSweep)
	require.True(t, snapshot.AllProofConfirmed)
	require.False(t, snapshot.Done)
}

func TestPlanSweepBroadcastedNotNeedSweep(t *testing.T) {
	planner, proof := newPlannerFixture(t,
		linearProofFixture(t),
	)

	confirmHeight := int32(100)
	sweepTxid := hashFromLabel("sweep")
	state := &State{
		ConfirmedTxids: []chainhash.Hash{
			proof.Layers()[0][0],
			proof.TargetOutpoint().Hash,
		},
		TargetConfirmHeight: fn.Some(confirmHeight),
		Sweep: SweepState{
			Status: SweepStatusBroadcasted,
			Txid:   fn.Some(sweepTxid),
		},
	}

	snapshot, err := planner.Plan(105, state)
	require.NoError(t, err)

	require.False(t, snapshot.NeedSweep)
	require.False(t, snapshot.Done)
	require.Equal(t, SweepStatusBroadcasted, snapshot.Sweep.Status)
	require.Equal(t, sweepTxid, snapshot.Sweep.Txid.UnwrapOrFail(t))
	require.True(t, snapshot.AllProofConfirmed)
}

func TestPlanSweepConfirmedDone(t *testing.T) {
	planner, proof := newPlannerFixture(t,
		linearProofFixture(t),
	)

	confirmHeight := int32(100)
	sweepHeight := int32(106)
	sweepTxid := hashFromLabel("sweep")
	state := &State{
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
	}

	snapshot, err := planner.Plan(106, state)
	require.NoError(t, err)

	require.True(t, snapshot.Done)
	require.False(t, snapshot.NeedSweep)
	require.Equal(t, SweepStatusConfirmed, snapshot.Sweep.Status)
	require.Equal(
		t, sweepHeight, snapshot.Sweep.ConfirmHeight.UnwrapOrFail(t),
	)
	require.True(t, snapshot.AllProofConfirmed)
}

func TestPlanInvalidStateDuplicateConfirmed(t *testing.T) {
	planner, _ := newPlannerFixture(t,
		linearProofFixture(t),
	)

	duplicate := hashFromLabel("dup")
	_, err := planner.Plan(100, &State{
		ConfirmedTxids: []chainhash.Hash{duplicate, duplicate},
	})
	require.ErrorContains(t, err, "duplicate confirmed txids")
}

func TestPlanInvalidStateUnknownTxid(t *testing.T) {
	planner, _ := newPlannerFixture(t,
		linearProofFixture(t),
	)

	_, err := planner.Plan(100, &State{
		ConfirmedTxids: []chainhash.Hash{hashFromLabel("unknown")},
	})
	require.ErrorContains(t, err, "is not in proof")
}

func TestPlanInvalidStateTargetHeightWithoutTargetConfirmation(t *testing.T) {
	planner, _ := newPlannerFixture(t,
		linearProofFixture(t),
	)

	confirmHeight := int32(100)
	_, err := planner.Plan(100, &State{
		TargetConfirmHeight: fn.Some(confirmHeight),
	})
	require.ErrorContains(
		t, err, "target confirm height set without confirmed target",
	)
}

func TestPlanInvalidSweepState(t *testing.T) {
	planner, _ := newPlannerFixture(t,
		linearProofFixture(t),
	)

	_, err := planner.Plan(100, &State{
		Sweep: SweepState{
			Status: SweepStatusBroadcasted,
		},
	})
	require.ErrorContains(t, err, "broadcasted sweep must have a txid")
}

func TestPlanInvalidStateDuplicateInFlight(t *testing.T) {
	planner, _ := newPlannerFixture(t,
		linearProofFixture(t),
	)

	duplicate := hashFromLabel("dup-inflight")
	_, err := planner.Plan(100, &State{
		InFlightTxids: []chainhash.Hash{duplicate, duplicate},
	})
	require.ErrorContains(t, err, "duplicate in-flight txids")
}

func TestPlanInvalidStateOverlapConfirmedAndInFlight(t *testing.T) {
	planner, proof := newPlannerFixture(t,
		linearProofFixture(t),
	)

	root := proof.Layers()[0][0]
	_, err := planner.Plan(100, &State{
		ConfirmedTxids: []chainhash.Hash{root},
		InFlightTxids:  []chainhash.Hash{root},
	})
	require.ErrorContains(t, err, "cannot be both confirmed and in-flight")
}

func TestPlanInvalidStateInFlightWithUnconfirmedParents(t *testing.T) {
	planner, proof := newPlannerFixture(t,
		threeLayerProofFixture(t),
	)

	middle := proof.Layers()[1][0]
	_, err := planner.Plan(100, &State{
		InFlightTxids: []chainhash.Hash{middle},
	})
	require.ErrorContains(t, err, "in-flight tx")
	require.ErrorContains(t, err, "unconfirmed parents")
}

func TestPlannerRejectsNilInputs(t *testing.T) {
	_, err := NewPlanner(nil)
	require.Error(t, err)

	planner, _ := newPlannerFixture(t,
		linearProofFixture(t),
	)

	_, err = planner.Plan(100, nil)
	require.Error(t, err)
}

func newPlannerFixture(t *testing.T,
	proof *recovery.Proof) (*Planner, *recovery.Proof) {

	t.Helper()

	planner, err := NewPlanner(proof)
	require.NoError(t, err)

	return planner, proof
}

func linearProofFixture(t *testing.T) *recovery.Proof {
	t.Helper()

	root := newTx(nil, 1, "root")
	rootTxid := root.TxHash()
	target := newTx([]wire.OutPoint{{
		Hash:  rootTxid,
		Index: 0,
	}}, 1, "target")

	proof, err := recovery.NewProof(
		wire.OutPoint{
			Hash:  target.TxHash(),
			Index: 0,
		},
		5,
		&recovery.Node{Kind: recovery.NodeKindTree, Tx: root},
		&recovery.Node{Kind: recovery.NodeKindTree, Tx: target},
	)
	require.NoError(t, err)

	return proof
}

func multiParentProofFixture(t *testing.T) *recovery.Proof {
	t.Helper()

	left := newTx(nil, 1, "left")
	right := newTx(nil, 1, "right")
	child := newTx([]wire.OutPoint{
		{
			Hash:  left.TxHash(),
			Index: 0,
		},
		{
			Hash:  right.TxHash(),
			Index: 0,
		},
	}, 1, "child")

	proof, err := recovery.NewProof(
		wire.OutPoint{
			Hash:  child.TxHash(),
			Index: 0,
		},
		6,
		&recovery.Node{Kind: recovery.NodeKindTree, Tx: left},
		&recovery.Node{Kind: recovery.NodeKindTree, Tx: right},
		&recovery.Node{Kind: recovery.NodeKindTree, Tx: child},
	)
	require.NoError(t, err)

	return proof
}

func threeLayerProofFixture(t *testing.T) *recovery.Proof {
	t.Helper()

	root := newTx(nil, 1, "root")
	middle := newTx([]wire.OutPoint{{
		Hash:  root.TxHash(),
		Index: 0,
	}}, 1, "middle")
	target := newTx([]wire.OutPoint{{
		Hash:  middle.TxHash(),
		Index: 0,
	}}, 1, "target")

	proof, err := recovery.NewProof(
		wire.OutPoint{
			Hash:  target.TxHash(),
			Index: 0,
		},
		7,
		&recovery.Node{Kind: recovery.NodeKindTree, Tx: root},
		&recovery.Node{Kind: recovery.NodeKindTree, Tx: middle},
		&recovery.Node{Kind: recovery.NodeKindTree, Tx: target},
	)
	require.NoError(t, err)

	return proof
}

func newTx(inputs []wire.OutPoint, numOutputs int, label string) *wire.MsgTx {
	tx := wire.NewMsgTx(2)

	for _, input := range inputs {
		in := wire.NewTxIn(&input, nil, nil)
		tx.AddTxIn(in)
	}

	for i := 0; i < numOutputs; i++ {
		tx.AddTxOut(&wire.TxOut{
			Value:    1,
			PkScript: []byte(fmt.Sprintf("%s-%d", label, i)),
		})
	}

	return tx
}

func hashFromLabel(label string) chainhash.Hash {
	return chainhash.HashH([]byte(label))
}
