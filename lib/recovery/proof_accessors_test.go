package recovery

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestNodeKindString checks the stable debug labels for NodeKind and also
// the unknown-kind path so future additions do not silently panic.
func TestNodeKindString(t *testing.T) {
	require.Equal(t, "tree", NodeKindTree.String())
	require.Equal(t, "checkpoint", NodeKindCheckpoint.String())
	require.Equal(t, "ark", NodeKindArk.String())
	require.Contains(t, NodeKind(99).String(), "unknown")
}

// TestTxStateString checks the debug labels for TxState.
func TestTxStateString(t *testing.T) {
	require.Equal(t, "pending", TxStatePending.String())
	require.Equal(t, "broadcasted", TxStateBroadcasted.String())
	require.Equal(t, "confirmed", TxStateConfirmed.String())
	require.Contains(t, TxState(99).String(), "unknown")
}

// TestSessionStatusString checks the debug labels for SessionStatus.
func TestSessionStatusString(t *testing.T) {
	require.Equal(t, "materializing", SessionStatusMaterializing.String())
	require.Equal(t, "awaiting_csv", SessionStatusAwaitingCSV.String())
	require.Equal(t, "sweep_ready", SessionStatusSweepReady.String())
	require.Equal(t, "failed", SessionStatusFailed.String())
	require.Contains(t, SessionStatus(99).String(), "unknown")
}

// TestNodeTXIDGuards checks both nil-receiver paths.
func TestNodeTXIDGuards(t *testing.T) {
	var nilNode *Node
	_, err := nilNode.TXID()
	require.ErrorContains(t, err, "node cannot be nil")

	_, err = (&Node{}).TXID()
	require.ErrorContains(t, err, "node tx cannot be nil")
}

// TestNodeOutputGuards exercises nil, missing-tx, and out-of-bounds paths.
func TestNodeOutputGuards(t *testing.T) {
	var nilNode *Node
	_, err := nilNode.Output(0)
	require.ErrorContains(t, err, "node cannot be nil")

	_, err = (&Node{}).Output(0)
	require.ErrorContains(t, err, "node tx cannot be nil")

	tx := wire.NewMsgTx(1)
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{0x51}})
	_, err = (&Node{Tx: tx}).Output(5)
	require.ErrorContains(t, err, "out of bounds")

	out, err := (&Node{Tx: tx}).Output(0)
	require.NoError(t, err)
	require.Equal(t, int64(1), out.Value)
}

// TestNodeAnchorOutputIndex covers absent anchors, one anchor, and the
// duplicate-anchor error path.
func TestNodeAnchorOutputIndex(t *testing.T) {
	var nilNode *Node
	_, _, err := nilNode.AnchorOutputIndex()
	require.ErrorContains(t, err, "node cannot be nil")

	_, _, err = (&Node{}).AnchorOutputIndex()
	require.ErrorContains(t, err, "node tx cannot be nil")

	tx := wire.NewMsgTx(1)
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{0x51}})
	_, ok, err := (&Node{Tx: tx}).AnchorOutputIndex()
	require.NoError(t, err)
	require.False(t, ok)

	anchored := wire.NewMsgTx(1)
	anchored.AddTxOut(&wire.TxOut{Value: 2, PkScript: []byte{0x51}})
	anchored.AddTxOut(arkscript.AnchorOutput())
	idx, ok, err := (&Node{Tx: anchored}).AnchorOutputIndex()
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, uint32(1), idx)

	duplicate := wire.NewMsgTx(1)
	duplicate.AddTxOut(arkscript.AnchorOutput())
	duplicate.AddTxOut(arkscript.AnchorOutput())
	_, _, err = (&Node{Tx: duplicate}).AnchorOutputIndex()
	require.ErrorContains(t, err, "multiple anchor outputs")
}

// TestNodeAnchorOutpoint exercises the composed AnchorOutpoint path.
func TestNodeAnchorOutpoint(t *testing.T) {
	tx := wire.NewMsgTx(1)
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{0x51}})
	tx.AddTxOut(arkscript.AnchorOutput())

	node := &Node{Tx: tx}
	op, ok, err := node.AnchorOutpoint()
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, uint32(1), op.Index)
	require.Equal(t, tx.TxHash(), op.Hash)

	var nilNode *Node
	_, _, err = nilNode.AnchorOutpoint()
	require.Error(t, err)

	noAnchor := wire.NewMsgTx(1)
	noAnchor.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{0x51}})
	_, ok, err = (&Node{Tx: noAnchor}).AnchorOutpoint()
	require.NoError(t, err)
	require.False(t, ok)
}

// TestProofAccessors exercises the public read-only accessors to lock in
// their error paths. Keep the proof intentionally small so the set-up cost is
// low and the test stays focused on accessor behavior.
func TestProofAccessors(t *testing.T) {
	root := makeProofTx('r', nil)
	target := makeProofTx('t', []wire.OutPoint{
		{Hash: root.TxHash(), Index: 0},
	})

	proof, err := NewProof(
		wire.OutPoint{
			Hash: target.TxHash(),
		},
		5, &Node{
			Kind: NodeKindTree,
			Tx:   root,
		}, &Node{
			Kind: NodeKindArk,
			Tx:   target,
		},
	)
	require.NoError(t, err)

	targetNode, err := proof.TargetNode()
	require.NoError(t, err)
	require.Equal(t, target.TxHash(), targetNode.Tx.TxHash())

	targetOut, err := proof.TargetOutput()
	require.NoError(t, err)
	require.Equal(t, int64('t')+1, targetOut.Value)

	roots := proof.RootTxids()
	require.Equal(t, []chainhash.Hash{root.TxHash()}, roots)

	layer, err := proof.Layer(root.TxHash())
	require.NoError(t, err)
	require.Equal(t, 0, layer)

	_, err = proof.Layer(chainhash.Hash{0xff})
	require.ErrorContains(t, err, "unknown txid")

	children, err := proof.ChildTxids(root.TxHash())
	require.NoError(t, err)
	require.Equal(t, []chainhash.Hash{target.TxHash()}, children)

	_, err = proof.ChildTxids(chainhash.Hash{0xff})
	require.ErrorContains(t, err, "unknown txid")
}
