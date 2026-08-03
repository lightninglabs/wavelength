package tapassets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/stretchr/testify/require"
)

// fakeTreeDriver fabricates deterministic commit results consistent with
// the materializer's fail-closed validation: composed output scripts derive
// from each requested internal key and a merkle root seeded by the input
// outpoint, so a whole tree materializes without tapd.
type fakeTreeDriver struct {
	commits int

	// mutate optionally corrupts a result before returning it.
	mutate func(request *tapsdk.CustomAnchorRequest, result *commitResult)
}

// merkleRootFor derives the fake's deterministic merkle root for one
// output.
func merkleRootFor(input wire.OutPoint, index uint32) tapsdk.Hash {
	seed := sha256.Sum256([]byte(fmt.Sprintf("root/%s/%d", input, index)))

	return tapsdk.Hash(seed)
}

// Preview is unused by the tree materializer.
func (f *fakeTreeDriver) Preview(context.Context, *tapsdk.CustomAnchorRequest,
	tapsdk.ConfirmedProofVerifier) ([]commitmentPreview, error) {

	return nil, fmt.Errorf("preview is not used by tree materialization")
}

// Commit echoes the request into a sealed-result projection, composing
// each asset output's script exactly the way tapd would.
func (f *fakeTreeDriver) Commit(_ context.Context,
	request *tapsdk.CustomAnchorRequest, _ tapsdk.ConfirmedProofVerifier) (
	*commitResult, error) {

	f.commits++

	template, err := psbtutil.Parse(request.AnchorPSBT)
	if err != nil {
		return nil, err
	}
	committedTx := template.UnsignedTx.Copy()
	input := committedTx.TxIn[0].PreviousOutPoint

	result := &commitResult{
		fundingMode: tapsdk.CustomAnchorFundingCallerFundedExact,
		inputs: []commitInput{{
			anchorOutpoint: sdkOutpoint(input),
			assetRef:       request.Inputs[0].AssetRef,
			amount:         request.Inputs[0].Amount,
		}},
	}

	for _, out := range request.Outputs {
		merkleRoot := merkleRootFor(input, out.AnchorOutputIndex)

		internalKey, err := btcec.ParsePubKey(
			out.Anchor.InternalKey.PubKey[:],
		)
		if err != nil {
			return nil, err
		}
		composed := txscript.ComputeTaprootOutputKey(
			internalKey, merkleRoot[:],
		)
		composedScript, err := txscript.PayToTaprootScript(composed)
		if err != nil {
			return nil, err
		}
		committedTx.TxOut[out.AnchorOutputIndex].PkScript =
			composedScript

		result.outputs = append(result.outputs, commitOutput{
			anchorOutputIndex: out.AnchorOutputIndex,
			anchorOutpoint: sdkOutpoint(wire.OutPoint{
				Hash:  committedTx.TxHash(),
				Index: out.AnchorOutputIndex,
			}),
			anchorValueSat:    int64(out.AnchorValueSat),
			assetRef:          out.AssetRef,
			amount:            out.Amount,
			taprootMerkleRoot: merkleRoot,
			scriptMode:        tapsdk.CustomAssetScriptOPTrue,
			opTrueWitness:     [][]byte{{0x51}},
			proofBlob: []byte(
				fmt.Sprintf("proof/%s/%d", input,
					out.AnchorOutputIndex),
			),
		})
	}

	// Re-derive per-output anchor outpoints now that every script (and
	// therefore the txid) is final.
	txid := committedTx.TxHash()
	for i := range result.outputs {
		result.outputs[i].anchorOutpoint = sdkOutpoint(wire.OutPoint{
			Hash:  txid,
			Index: result.outputs[i].anchorOutputIndex,
		})
	}

	committedPacket, err := psbtutil.Parse(request.AnchorPSBT)
	if err != nil {
		return nil, err
	}
	committedPacket.UnsignedTx = committedTx
	anchorBytes, err := psbtutil.Serialize(committedPacket)
	if err != nil {
		return nil, err
	}
	result.anchorPSBT = anchorBytes
	result.packageBytes = []byte(fmt.Sprintf("package/%s", input))

	if f.mutate != nil {
		f.mutate(request, result)
	}

	return result, nil
}

// DecodePackage is unused by the tree materializer.
func (f *fakeTreeDriver) DecodePackage([]byte) (*commitResult, error) {
	return nil, fmt.Errorf("decode is not used by tree materialization")
}

// treeMaterializerFixture bundles a built structure with a materializer
// wired to the fake driver.
type treeMaterializerFixture struct {
	structure *tree.Structure
	mat       *TreeMaterializer
	driver    *fakeTreeDriver
	batch     wire.OutPoint
	batchOut  *wire.TxOut
	rootTweak []byte
}

// newTreeMaterializerFixture builds a three-leaf asset tree structure and a
// materializer whose root source binds to a synthetic batch output.
func newTreeMaterializerFixture(t *testing.T,
	amounts ...uint64) *treeMaterializerFixture {

	t.Helper()

	_, operatorKey := createTestPubKey(t)
	leaves := make([]tree.LeafDescriptor, len(amounts))
	for i, amount := range amounts {
		_, owner := createTestPubKey(t)
		leaves[i] = tree.LeafDescriptor{
			PkScript: []byte{
				0x51,
				byte(i),
			},
			Amount:      10_000,
			CoSignerKey: owner,
			AssetAmount: amount,
		}
	}

	structure, err := tree.BuildStructure(leaves, tree.StructureConfig{
		OperatorKey: operatorKey,
		Radix:       2,
	})
	require.NoError(t, err)
	require.NotNil(t, structure.AssetContext)

	sweepLeaf := txscript.NewBaseTapLeaf([]byte{0x51, 0xB2})

	// The batch output script must reproduce from the root cosigners
	// and the root signing tweak, mirroring a real batch commit.
	rootTweak := bytes.Repeat([]byte{0x0C}, 32)
	rootInternal, err := tree.ComputeInternalKey(
		structure.Root.CoSigners,
	)
	require.NoError(t, err)
	batchKey := txscript.ComputeTaprootOutputKey(rootInternal, rootTweak)
	batchScript, err := txscript.PayToTaprootScript(batchKey)
	require.NoError(t, err)

	batchOutpoint := wire.OutPoint{Hash: chainhash.Hash{0xBA}, Index: 1}

	driver := &fakeTreeDriver{}
	assetRef := tapsdk.AssetRefFromAssetID(tapsdk.AssetID{0xAA})

	mat := &TreeMaterializer{
		cfg: TreeMaterializerConfig{
			AssetRef:     assetRef,
			AssetContext: structure.AssetContext,
			SweepLeaf:    sweepLeaf,
			LeafAnchor: func(node *tree.Node) (TreeLeafAnchor,
				error) {

				_, leafInternal := createTestPubKey(t)
				script, err := txscript.PayToTaprootScript(
					leafInternal,
				)
				if err != nil {
					return TreeLeafAnchor{}, err
				}

				return TreeLeafAnchor{
					UncomposedPkScript: script,
					InternalKey:        leafInternal,
					TapLeaves: []txscript.TapLeaf{
						txscript.NewBaseTapLeaf(
							[]byte{0x51},
						),
					},
				}, nil
			},
			Root: TreeRootAssetSource{
				ProofFile: []byte{
					0x01,
				},
				Witness: [][]byte{
					{
						0x51,
					},
				},
				SigningTweak:  rootTweak,
				BatchPkScript: batchScript,
			},
			Digest: tapsdk.Hash{
				0xD1,
			},
		},
		driver:   driver,
		handoffs: make(map[wire.OutPoint]*nodeAssetHandoff),
	}

	return &treeMaterializerFixture{
		structure: structure,
		mat:       mat,
		driver:    driver,
		batch:     batchOutpoint,
		batchOut:  wire.NewTxOut(30_000, batchScript),
		rootTweak: rootTweak,
	}
}

// createTestPubKey returns a fresh keypair for fixtures.
func createTestPubKey(t *testing.T) (*btcec.PrivateKey, *btcec.PublicKey) {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return priv, priv.PubKey()
}

// TestTreeMaterializerBuildsAssetTree materializes a full three-leaf tree
// against the fake driver and checks every binding the design relies on.
func TestTreeMaterializerBuildsAssetTree(t *testing.T) {
	t.Parallel()

	f := newTreeMaterializerFixture(t, 800, 200, 500)

	err := tree.Materialize(
		t.Context(), f.structure.Root, tree.MaterializeParams{
			Input: f.batch,
		},
		f.mat,
	)
	require.NoError(t, err)

	// One commit per node: 3 leaves at radix 2 form 5 transactions.
	require.Equal(t, 5, f.driver.commits)

	// The materialized tree must be structurally consistent: every
	// child input references its parent output.
	built := &tree.Tree{
		Root:          f.structure.Root,
		BatchOutpoint: f.batch,
		BatchOutput:   f.batchOut,
	}
	require.NoError(t, built.Verify())

	assetCtx := f.structure.AssetContext
	require.Equal(t, f.rootTweak, assetCtx.SigningTweak(f.batch))

	for node := range f.structure.Root.NodesIter() {
		// Every node carries its committed outputs plus the P2A
		// anchor, its bound final key, and its recorded material.
		require.NotEmpty(t, node.Outputs)
		require.NotNil(t, node.FinalKey)
		require.NotEmpty(t, assetCtx.SigningTweak(node.Input))
		require.NotEmpty(t, assetCtx.SealedPackage(node.Input))

		expectedOutputs := len(node.Children) + 1
		if len(node.Children) == 0 {
			expectedOutputs = 2
		}
		require.Len(t, node.Outputs, expectedOutputs)

		// Children sign with their parent output's merkle root.
		for idx, child := range node.Children {
			require.Equal(
				t, merkleRootFor(node.Input, idx),
				tapsdk.Hash(
					assetCtx.SigningTweak(
						child.Input,
					),
				),
			)
		}
	}

	// The root's final key must reproduce the batch output script.
	rootScript, err := txscript.PayToTaprootScript(
		f.structure.Root.FinalKey,
	)
	require.NoError(t, err)
	require.Equal(t, f.batchOut.PkScript, rootScript)
}

// TestTreeMaterializerRejectsZeroAmountChild ensures a subtree without
// asset units fails closed instead of committing an empty transition.
func TestTreeMaterializerRejectsZeroAmountChild(t *testing.T) {
	t.Parallel()

	f := newTreeMaterializerFixture(t, 800, 0)

	err := tree.Materialize(
		t.Context(), f.structure.Root, tree.MaterializeParams{
			Input: f.batch,
		},
		f.mat,
	)
	require.ErrorContains(t, err, "carries no asset amount")
}

// TestTreeMaterializerRejectsUnknownInput ensures a node whose input has
// no recorded handoff (and is not the root) fails closed.
func TestTreeMaterializerRejectsUnknownInput(t *testing.T) {
	t.Parallel()

	f := newTreeMaterializerFixture(t, 800, 200)
	f.mat.cfg.Root = TreeRootAssetSource{}

	err := tree.Materialize(
		t.Context(), f.structure.Root, tree.MaterializeParams{
			Input: f.batch,
		},
		f.mat,
	)
	require.ErrorContains(t, err, "root asset source is incomplete")
}

// TestTreeMaterializerRejectsTamperedCommit ensures every deviation of the
// committed result from the pinned template fails closed.
func TestTreeMaterializerRejectsTamperedCommit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*tapsdk.CustomAnchorRequest, *commitResult)
		wantErr string
	}{
		{
			name: "nonzero fee",
			mutate: func(_ *tapsdk.CustomAnchorRequest,
				result *commitResult) {

				result.actualFeeSat = 10
			},
			wantErr: "must be zero fee",
		},
		{
			name: "input outpoint drift",
			mutate: func(_ *tapsdk.CustomAnchorRequest,
				result *commitResult) {

				result.inputs[0].anchorOutpoint.Index++
			},
			wantErr: "outpoint mismatch",
		},
		{
			name: "script mode downgrade",
			mutate: func(_ *tapsdk.CustomAnchorRequest,
				result *commitResult) {

				result.outputs[0].scriptMode =
					tapsdk.CustomAssetScriptWallet
			},
			wantErr: "not OP_TRUE",
		},
		{
			name: "merkle root drift",
			mutate: func(_ *tapsdk.CustomAnchorRequest,
				result *commitResult) {

				result.outputs[0].taprootMerkleRoot[0] ^= 1
			},
			wantErr: "does not reproduce",
		},
		{
			name: "asset amount drift",
			mutate: func(_ *tapsdk.CustomAnchorRequest,
				result *commitResult) {

				result.outputs[0].amount++
			},
			wantErr: "asset amount",
		},
		{
			name: "topology drift",
			mutate: func(_ *tapsdk.CustomAnchorRequest,
				result *commitResult) {

				packet, err := psbtutil.Parse(
					result.anchorPSBT,
				)
				if err != nil {
					panic(err)
				}
				packet.UnsignedTx.TxOut[len(
					packet.UnsignedTx.TxOut,
				)-1].Value = 1
				encoded, err := psbtutil.Serialize(packet)
				if err != nil {
					panic(err)
				}
				result.anchorPSBT = encoded
			},
			wantErr: "diverges from the node template",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f := newTreeMaterializerFixture(t, 800, 200)
			f.driver.mutate = test.mutate

			err := tree.Materialize(
				t.Context(), f.structure.Root,
				tree.MaterializeParams{
					Input: f.batch,
				},
				f.mat,
			)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}
