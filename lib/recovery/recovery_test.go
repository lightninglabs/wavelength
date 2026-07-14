package recovery

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestProofLayersMergeParents verifies that a proof can represent a
// multi-input merge and exposes deterministic layers.
func TestProofLayersMergeParents(t *testing.T) {
	rootATx := makeRecoveryTx('a', []wire.OutPoint{
		makeExternalOutpoint('x', 0),
	}, true)
	rootBTx := makeRecoveryTx('b', []wire.OutPoint{
		makeExternalOutpoint('y', 0),
	}, true)

	rootATxid := rootATx.TxHash()
	rootBTxid := rootBTx.TxHash()

	mergeTx := makeRecoveryTx('m', []wire.OutPoint{
		{Hash: rootATxid, Index: 0},
		{Hash: rootBTxid, Index: 0},
	}, true)

	proof, err := NewProof(
		wire.OutPoint{
			Hash:  mergeTx.TxHash(),
			Index: 0,
		},
		5, &Node{
			Kind: NodeKindCheckpoint,
			Tx:   rootATx,
		}, &Node{
			Kind: NodeKindCheckpoint,
			Tx:   rootBTx,
		}, &Node{
			Kind: NodeKindArk,
			Tx:   mergeTx,
		},
	)
	require.NoError(t, err)

	layers := proof.Layers()
	require.Len(t, layers, 2)
	require.ElementsMatch(t, []chainhash.Hash{
		rootATxid, rootBTxid,
	}, layers[0])
	require.Equal(t, []chainhash.Hash{mergeTx.TxHash()}, layers[1])

	parentTxids, err := proof.ParentTxids(mergeTx.TxHash())
	require.NoError(t, err)
	require.ElementsMatch(t, []chainhash.Hash{
		rootATxid, rootBTxid,
	}, parentTxids)
}

// TestProofLayersNestedMergeParents verifies that a proof can represent a
// nested ancestry graph where one target parent is itself a two-parent merge.
func TestProofLayersNestedMergeParents(t *testing.T) {
	session := newNestedMergeSession(t)
	proof := session.Proof()

	layers := proof.Layers()
	require.Len(t, layers, 3)
	require.Len(t, layers[0], 3)
	require.Len(t, layers[1], 1)
	require.Len(t, layers[2], 1)

	mergeBCTxid := layers[1][0]
	targetTxid := layers[2][0]

	targetParents, err := proof.ParentTxids(targetTxid)
	require.NoError(t, err)
	require.Len(t, targetParents, 2)
	require.Contains(t, targetParents, mergeBCTxid)

	var rootATxid chainhash.Hash
	for _, parent := range targetParents {
		if parent == mergeBCTxid {
			continue
		}

		rootATxid = parent
	}
	require.Contains(t, layers[0], rootATxid)

	mergeParents, err := proof.ParentTxids(mergeBCTxid)
	require.NoError(t, err)
	require.Len(t, mergeParents, 2)
	require.NotContains(t, mergeParents, rootATxid)
	require.Subset(t, layers[0], mergeParents)
}

// TestSessionTracksMultiParentReadiness verifies that the session only
// releases a merge transaction once all in-proof parents confirm.
func TestSessionTracksMultiParentReadiness(t *testing.T) {
	session := newMergeSession(t)

	snapshot, err := session.SnapshotAt(100)
	require.NoError(t, err)
	require.Equal(t, SessionStatusMaterializing, snapshot.Status)
	require.Len(t, snapshot.ReadyToBroadcast, 2)
	require.Empty(t, snapshot.AwaitingConfirmation)
	require.Len(t, snapshot.Blocked, 1)
	require.Len(t, snapshot.Blocked[0].MissingParents, 2)

	rootATxid := snapshot.ReadyToBroadcast[0].Txid
	rootBTxid := snapshot.ReadyToBroadcast[1].Txid
	if rootATxid.String() > rootBTxid.String() {
		rootATxid, rootBTxid = rootBTxid, rootATxid
	}

	require.NoError(t, session.MarkBroadcasted(rootATxid))
	snapshot, err = session.SnapshotAt(100)
	require.NoError(t, err)
	require.Equal(
		t, []chainhash.Hash{rootATxid}, snapshot.AwaitingConfirmation,
	)
	require.Len(t, snapshot.ReadyToBroadcast, 1)

	require.NoError(t, session.MarkConfirmed(rootATxid, 101))
	snapshot, err = session.SnapshotAt(101)
	require.NoError(t, err)
	require.Len(t, snapshot.ReadyToBroadcast, 1)
	require.Equal(t, rootBTxid, snapshot.ReadyToBroadcast[0].Txid)
	require.Len(t, snapshot.Blocked, 1)
	require.Equal(
		t, []chainhash.Hash{rootBTxid},
		snapshot.Blocked[0].MissingParents,
	)

	require.NoError(t, session.MarkBroadcasted(rootBTxid))
	require.NoError(t, session.MarkConfirmed(rootBTxid, 102))

	snapshot, err = session.SnapshotAt(102)
	require.NoError(t, err)
	require.Len(t, snapshot.ReadyToBroadcast, 1)
	mergeTxid := snapshot.ReadyToBroadcast[0].Txid

	require.NoError(t, session.MarkBroadcasted(mergeTxid))
	require.NoError(t, session.MarkConfirmed(mergeTxid, 103))

	snapshot, err = session.SnapshotAt(107)
	require.NoError(t, err)
	require.Equal(t, SessionStatusAwaitingCSV, snapshot.Status)
	csv := snapshot.CSV.UnwrapOrFail(t)
	require.Equal(t, int32(108), csv.MaturityHeight)
	require.Equal(t, int32(1), csv.BlocksRemaining)

	snapshot, err = session.SnapshotAt(108)
	require.NoError(t, err)
	require.Equal(t, SessionStatusSweepReady, snapshot.Status)
	require.True(t, snapshot.CSV.UnwrapOrFail(t).Ready)
	require.Empty(t, snapshot.ReadyToBroadcast)
	require.Empty(t, snapshot.AwaitingConfirmation)
}

// TestSessionTracksNestedParentReadiness verifies that the session handles
// a target whose parents come from different origins and nested merges.
func TestSessionTracksNestedParentReadiness(t *testing.T) {
	session := newNestedMergeSession(t)
	proof := session.Proof()
	layers := proof.Layers()

	require.Len(t, layers, 3)
	require.Len(t, layers[0], 3)

	mergeBCTxid := layers[1][0]
	targetTxid := layers[2][0]

	targetParents, err := proof.ParentTxids(targetTxid)
	require.NoError(t, err)

	var rootATxid chainhash.Hash
	for _, parent := range targetParents {
		if parent == mergeBCTxid {
			continue
		}

		rootATxid = parent
	}

	mergeBCParents, err := proof.ParentTxids(mergeBCTxid)
	require.NoError(t, err)
	require.Len(t, mergeBCParents, 2)

	snapshot, err := session.SnapshotAt(200)
	require.NoError(t, err)
	require.Equal(t, SessionStatusMaterializing, snapshot.Status)
	require.ElementsMatch(
		t, layers[0], readyActionTxids(snapshot.ReadyToBroadcast),
	)

	targetBlocked := blockedActionForTxid(t, snapshot.Blocked, targetTxid)
	require.ElementsMatch(t, targetParents,
		targetBlocked.MissingParents)

	mergeBlocked := blockedActionForTxid(t, snapshot.Blocked, mergeBCTxid)
	require.ElementsMatch(t, mergeBCParents, mergeBlocked.MissingParents)

	require.NoError(t, session.MarkBroadcasted(rootATxid))
	require.NoError(t, session.MarkConfirmed(rootATxid, 201))

	snapshot, err = session.SnapshotAt(201)
	require.NoError(t, err)
	require.ElementsMatch(
		t, mergeBCParents, readyActionTxids(snapshot.ReadyToBroadcast),
	)

	targetBlocked = blockedActionForTxid(t, snapshot.Blocked, targetTxid)
	require.Equal(
		t, []chainhash.Hash{mergeBCTxid}, targetBlocked.MissingParents,
	)

	mergeBlocked = blockedActionForTxid(t, snapshot.Blocked, mergeBCTxid)
	require.ElementsMatch(t, mergeBCParents, mergeBlocked.MissingParents)

	for _, parentTxid := range mergeBCParents {
		require.NoError(t, session.MarkBroadcasted(parentTxid))
		require.NoError(t, session.MarkConfirmed(parentTxid, 202))
	}

	snapshot, err = session.SnapshotAt(202)
	require.NoError(t, err)
	require.Equal(
		t, []chainhash.Hash{mergeBCTxid},
		readyActionTxids(snapshot.ReadyToBroadcast),
	)

	targetBlocked = blockedActionForTxid(t, snapshot.Blocked, targetTxid)
	require.Equal(
		t, []chainhash.Hash{mergeBCTxid}, targetBlocked.MissingParents,
	)

	require.NoError(t, session.MarkBroadcasted(mergeBCTxid))
	require.NoError(t, session.MarkConfirmed(mergeBCTxid, 203))

	snapshot, err = session.SnapshotAt(203)
	require.NoError(t, err)
	require.Equal(
		t, []chainhash.Hash{targetTxid},
		readyActionTxids(snapshot.ReadyToBroadcast),
	)
	require.Empty(t, snapshot.Blocked)

	require.NoError(t, session.MarkBroadcasted(targetTxid))
	require.NoError(t, session.MarkConfirmed(targetTxid, 204))

	snapshot, err = session.SnapshotAt(208)
	require.NoError(t, err)
	require.Equal(t, SessionStatusAwaitingCSV, snapshot.Status)
	csv := snapshot.CSV.UnwrapOrFail(t)
	require.Equal(t, int32(209), csv.MaturityHeight)
	require.Equal(t, int32(1), csv.BlocksRemaining)

	snapshot, err = session.SnapshotAt(209)
	require.NoError(t, err)
	require.Equal(t, SessionStatusSweepReady, snapshot.Status)
	require.True(t, snapshot.CSV.UnwrapOrFail(t).Ready)
}

// TestSessionRejectsBroadcastBeforeParentsConfirmed verifies that the
// session enforces dependency order for multi-input nodes.
func TestSessionRejectsBroadcastBeforeParentsConfirmed(t *testing.T) {
	session := newMergeSession(t)

	proof := session.Proof()
	layers := proof.Layers()
	require.Len(t, layers, 2)
	mergeTxid := layers[1][0]

	err := session.MarkBroadcasted(mergeTxid)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not ready")
}

// TestProofRejectsUnrelatedNode verifies that a proof cannot contain nodes
// that do not contribute to the target.
func TestProofRejectsUnrelatedNode(t *testing.T) {
	rootTx := makeRecoveryTx('r', []wire.OutPoint{
		makeExternalOutpoint('u', 0),
	}, true)
	targetTx := makeRecoveryTx('t', []wire.OutPoint{
		{Hash: rootTx.TxHash(), Index: 0},
	}, true)
	unrelatedTx := makeRecoveryTx('z', []wire.OutPoint{
		makeExternalOutpoint('v', 0),
	}, true)

	_, err := NewProof(
		wire.OutPoint{
			Hash:  targetTx.TxHash(),
			Index: 0,
		},
		1, &Node{
			Kind: NodeKindTree,
			Tx:   rootTx,
		}, &Node{
			Kind: NodeKindArk,
			Tx:   targetTx,
		}, &Node{
			Kind: NodeKindCheckpoint,
			Tx:   unrelatedTx,
		},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not contribute")
}

// TestSessionExportStateRestoresProgress verifies that callers can persist and
// restore pure recovery progress without a hydration-oriented manager object.
func TestSessionExportStateRestoresProgress(t *testing.T) {
	session := newNestedMergeSession(t)
	proof := session.Proof()

	initial, err := session.SnapshotAt(100)
	require.NoError(t, err)
	require.Len(t, initial.ReadyToBroadcast, 3)

	rootATxid := initial.ReadyToBroadcast[0].Txid
	require.NoError(t, session.MarkBroadcasted(rootATxid))
	require.NoError(t, session.MarkConfirmed(rootATxid, 101))

	exported := session.ExportState()
	restored, err := NewSessionFromState(proof, exported)
	require.NoError(t, err)

	restoredSnapshot, err := restored.SnapshotAt(101)
	require.NoError(t, err)
	require.Equal(t, SessionStatusMaterializing,
		restoredSnapshot.Status)

	targetTxid := proof.TargetOutpoint().Hash
	targetBlocked := blockedActionForTxid(
		t, restoredSnapshot.Blocked, targetTxid,
	)
	require.NotEmpty(t, targetBlocked.MissingParents)
}

// TestSessionRestoreRejectsInvalidState verifies that state import rejects
// missing or inconsistent node progress.
func TestSessionRestoreRejectsInvalidState(t *testing.T) {
	session := newMergeSession(t)
	proof := session.Proof()
	state := session.ExportState()

	for txid := range state.TxStates {
		delete(state.TxStates, txid)
		break
	}

	_, err := NewSessionFromState(proof, state)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing tx state")
}

// TestSessionRestorePreservesFailure verifies that terminal node failures
// survive a restart round-trip.
func TestSessionRestorePreservesFailure(t *testing.T) {
	session := newMergeSession(t)
	initial, err := session.SnapshotAt(50)
	require.NoError(t, err)
	require.Len(t, initial.ReadyToBroadcast, 2)

	failedTxid := initial.ReadyToBroadcast[0].Txid
	require.NoError(
		t,
		session.MarkFailed(
			failedTxid, fmt.Errorf("package rejected"),
		),
	)

	restored, err := NewSessionFromState(
		session.Proof(), session.ExportState(),
	)
	require.NoError(t, err)

	snapshot, err := restored.SnapshotAt(50)
	require.NoError(t, err)
	require.Equal(t, SessionStatusFailed, snapshot.Status)
	require.Equal(t, failedTxid, snapshot.FailedTxid.UnwrapOrFail(t))
	require.Error(t, snapshot.LastError)
	require.Contains(t, snapshot.LastError.Error(), "package rejected")
}

// makeExternalOutpoint constructs a stable outpoint that is not part of the
// proof graph.
func makeExternalOutpoint(tag byte, index uint32) wire.OutPoint {
	hash := chainhash.Hash{}
	hash[0] = tag
	hash[1] = byte(index)

	return wire.OutPoint{
		Hash:  hash,
		Index: index,
	}
}

// makeRecoveryTx constructs a deterministic recovery transaction for tests.
func makeRecoveryTx(tag byte, prevOuts []wire.OutPoint,
	withAnchor bool) *wire.MsgTx {

	tx := wire.NewMsgTx(3)
	for _, prevOut := range prevOuts {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: prevOut,
			Sequence:         wire.MaxTxInSequenceNum,
		})
	}

	tx.AddTxOut(&wire.TxOut{
		Value:    int64(tag) + 1,
		PkScript: []byte{0x51, tag},
	})

	if withAnchor {
		tx.AddTxOut(arkscript.AnchorOutput())
	}

	return tx
}

// newMergeSession constructs a reusable multi-parent merge session.
func newMergeSession(t *testing.T) *Session {
	t.Helper()

	rootATx := makeRecoveryTx('a', []wire.OutPoint{
		makeExternalOutpoint('x', 0),
	}, true)
	rootBTx := makeRecoveryTx('b', []wire.OutPoint{
		makeExternalOutpoint('y', 0),
	}, true)

	mergeTx := makeRecoveryTx('m', []wire.OutPoint{
		{Hash: rootATx.TxHash(), Index: 0},
		{Hash: rootBTx.TxHash(), Index: 0},
	}, true)

	proof, err := NewProof(
		wire.OutPoint{
			Hash:  mergeTx.TxHash(),
			Index: 0,
		},
		5, &Node{
			Kind: NodeKindCheckpoint,
			Tx:   rootATx,
		}, &Node{
			Kind: NodeKindCheckpoint,
			Tx:   rootBTx,
		}, &Node{
			Kind: NodeKindArk,
			Tx:   mergeTx,
		},
	)
	require.NoError(t, err)

	session, err := NewSession(proof)
	require.NoError(t, err)

	return session
}

// newNestedMergeSession constructs a reusable nested ancestry session.
func newNestedMergeSession(t *testing.T) *Session {
	t.Helper()

	rootATx := makeRecoveryTx('a', []wire.OutPoint{
		makeExternalOutpoint('x', 0),
	}, true)
	rootBTx := makeRecoveryTx('b', []wire.OutPoint{
		makeExternalOutpoint('y', 0),
	}, true)
	rootCTx := makeRecoveryTx('c', []wire.OutPoint{
		makeExternalOutpoint('z', 0),
	}, true)

	mergeBCTx := makeRecoveryTx('d', []wire.OutPoint{
		{Hash: rootBTx.TxHash(), Index: 0},
		{Hash: rootCTx.TxHash(), Index: 0},
	}, true)

	targetTx := makeRecoveryTx('t', []wire.OutPoint{
		{Hash: rootATx.TxHash(), Index: 0},
		{Hash: mergeBCTx.TxHash(), Index: 0},
	}, true)

	proof, err := NewProof(
		wire.OutPoint{
			Hash:  targetTx.TxHash(),
			Index: 0,
		},
		5, &Node{
			Kind: NodeKindTree,
			Tx:   rootATx,
		}, &Node{
			Kind: NodeKindTree,
			Tx:   rootBTx,
		}, &Node{
			Kind: NodeKindTree,
			Tx:   rootCTx,
		}, &Node{
			Kind: NodeKindArk,
			Tx:   mergeBCTx,
		}, &Node{
			Kind: NodeKindArk,
			Tx:   targetTx,
		},
	)
	require.NoError(t, err)

	session, err := NewSession(proof)
	require.NoError(t, err)

	return session
}

// readyActionTxids collects the txids in a ready-to-broadcast list.
func readyActionTxids(actions []BroadcastAction) []chainhash.Hash {
	txids := make([]chainhash.Hash, 0, len(actions))
	for _, action := range actions {
		txids = append(txids, action.Txid)
	}

	return txids
}

// blockedActionForTxid returns the blocked action for the requested txid.
func blockedActionForTxid(t *testing.T, actions []BlockedAction,
	txid chainhash.Hash) BlockedAction {

	t.Helper()

	for _, action := range actions {
		if action.Txid == txid {
			return action
		}
	}

	require.Failf(t, "blocked action missing", "txid=%s", txid)

	return BlockedAction{}
}
