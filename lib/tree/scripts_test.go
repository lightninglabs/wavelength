package tree

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestValidateChildScriptsBitcoin verifies that Bitcoin tree edges use the
// common sweep tapscript root when deriving each child's funding script.
func TestValidateChildScriptsBitcoin(t *testing.T) {
	t.Parallel()

	_, operatorKey := createTestKey(t)
	leaves := make([]LeafDescriptor, 4)
	ownerKeys := make([]*btcec.PublicKey, 4)
	for i := range leaves {
		_, ownerKey := createTestKey(t)
		ownerKeys[i] = ownerKey
		leaves[i] = LeafDescriptor{
			Amount: 10_000,
			PkScript: []byte{
				txscript.OP_TRUE, byte(i),
			},
			CoSignerKey: ownerKey,
		}
	}

	tree, err := NewTree(
		wire.OutPoint{
			Hash: chainhash.Hash{0x01},
		}, &wire.TxOut{
			Value: sumLeafAmounts(leaves),
		},
		leaves,
		operatorKey,
		bytes.Repeat(
			[]byte{0x02}, 32,
		),
		2,
	)
	require.NoError(t, err)
	require.NoError(t, tree.ValidateChildScripts())
	require.False(t, tree.Root.Children[0].IsLeaf())

	_, substitutedKey := createTestKey(t)
	substitutedScript, err := txscript.PayToTaprootScript(substitutedKey)
	require.NoError(t, err)
	tree.Root.Outputs[0].PkScript = substitutedScript
	relinkChildInputs(t, tree.Root)

	err = tree.ValidateChildScripts()
	require.ErrorContains(t, err, "does not match child cosigners")

	_, err = tree.ValidatePath(ownerKeys[0], leaves[0], operatorKey)
	require.ErrorContains(t, err, "child scripts invalid")
}

// TestValidateChildScriptsAsset verifies that asset tree edges use the
// child's per-node signing tweak instead of the Bitcoin sweep root.
func TestValidateChildScriptsAsset(t *testing.T) {
	t.Parallel()

	_, rootKey := createTestKey(t)
	_, childKey := createTestKey(t)
	childTweak := bytes.Repeat([]byte{0x03}, 32)
	childFinalKey, err := ComputeFinalKey(
		[]*btcec.PublicKey{childKey}, childTweak,
	)
	require.NoError(t, err)
	childScript, err := txscript.PayToTaprootScript(childFinalKey)
	require.NoError(t, err)

	root := &Node{
		Input: wire.OutPoint{
			Hash: chainhash.Hash{
				0x04,
			},
		},
		Outputs: []*wire.TxOut{
			{
				Value:    10_000,
				PkScript: childScript,
			},
			arkscript.AnchorOutput(),
		},
		CoSigners: []*btcec.PublicKey{
			rootKey,
		},
		Children: make(map[uint32]*Node),
	}
	rootTxID, err := root.TXID()
	require.NoError(t, err)
	child := &Node{
		Input: wire.OutPoint{
			Hash: rootTxID,
		},
		Outputs: []*wire.TxOut{
			{
				Value: 10_000,
				PkScript: []byte{
					txscript.OP_TRUE,
				},
			},
			arkscript.AnchorOutput(),
		},
		CoSigners: []*btcec.PublicKey{
			childKey,
		},
		Children: make(map[uint32]*Node),
	}
	root.Children[0] = child

	assetContext := NewAssetTreeContext()
	assetContext.SetAssetRef("asset")
	assetContext.SetNodeAssetAmount(root, 1)
	assetContext.SetNodeAssetAmount(child, 1)
	assetContext.SetSigningTweak(root.Input, bytes.Repeat([]byte{0x05}, 32))
	assetContext.SetSigningTweak(child.Input, childTweak)
	assetContext.SetLeafAssetRoot(
		child.Input,
		bytes.Repeat(
			[]byte{0x06}, 32,
		),
	)

	tree := &Tree{
		Root:               root,
		SweepTapscriptRoot: bytes.Repeat([]byte{0x07}, 32),
		AssetContext:       assetContext,
	}
	require.NoError(t, tree.ValidateChildScripts())

	wrongFinalKey, err := ComputeFinalKey(
		child.CoSigners, tree.SweepTapscriptRoot,
	)
	require.NoError(t, err)
	root.Outputs[0].PkScript, err = txscript.PayToTaprootScript(
		wrongFinalKey,
	)
	require.NoError(t, err)

	err = tree.ValidateChildScripts()
	require.ErrorContains(t, err, "does not match child cosigners")
}

// relinkChildInputs updates each child input after a parent transaction is
// changed by a hostile test fixture.
func relinkChildInputs(t *testing.T, node *Node) {
	t.Helper()

	txid, err := node.TXID()
	require.NoError(t, err)
	for outputIdx, child := range node.Children {
		child.Input = wire.OutPoint{
			Hash:  txid,
			Index: outputIdx,
		}
		relinkChildInputs(t, child)
	}
}
