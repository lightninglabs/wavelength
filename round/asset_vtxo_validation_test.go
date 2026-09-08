package round

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

type recordingAssetVTXOVerifier struct {
	calls         int
	assetRef      string
	assetAmount   uint64
	commitmentTx  *wire.MsgTx
	clientTree    *tree.Tree
	sealedPackage []byte
	err           error
}

type recordingOwnedScriptRegistrar struct {
	pkScript []byte
	ownerKey keychain.KeyDescriptor
}

func (r *recordingOwnedScriptRegistrar) RegisterOwnedScript(_ context.Context,
	pkScript []byte, ownerKey keychain.KeyDescriptor) error {

	r.pkScript = append([]byte(nil), pkScript...)
	r.ownerKey = ownerKey

	return nil
}

func (v *recordingAssetVTXOVerifier) VerifyAssetVTXO(_ context.Context,
	assetRef string, assetAmount uint64, commitmentTx *wire.MsgTx,
	clientTree *tree.Tree, sealedPackage []byte) error {

	v.calls++
	v.assetRef = assetRef
	v.assetAmount = assetAmount
	v.commitmentTx = commitmentTx
	v.clientTree = clientTree
	v.sealedPackage = append([]byte(nil), sealedPackage...)

	return v.err
}

type boundAssetVTXOFixture struct {
	intent        BoardingIntent
	request       types.VTXORequest
	commitmentTx  *psbt.Packet
	tree          *tree.Tree
	leafOutpoint  wire.OutPoint
	sealedPackage []byte
}

func newBoundAssetVTXOFixture(t *testing.T,
	h *boardingTestHarness) *boundAssetVTXOFixture {

	t.Helper()

	intent := h.newTestBoardingIntent()
	request := h.newTestVTXORequestForIntent(intent)
	request.AssetRef = tapsdk.AssetRefFromAssetID(
		tapsdk.AssetID{1},
	).String()
	request.AssetAmount = 500
	request.FixedAmount = true

	assetRoot := chainhash.HashH([]byte("leaf asset root"))
	policy, err := request.DecodePolicyTemplate()
	require.NoError(t, err)
	compiled, err := policy.Compile()
	require.NoError(t, err)
	composed, err := arkscript.ComposeWithSiblingRoot(
		compiled, assetRoot,
	)
	require.NoError(t, err)
	leafScript, err := txscript.PayToTaprootScript(composed.OutputKey())
	require.NoError(t, err)

	rootTweak := chainhash.HashH([]byte("batch asset root"))
	rootKey, err := tree.ComputeFinalKey(
		tree.UniqueCosigners(
			[]*btcec.PublicKey{
				h.clientPubKey, h.operatorPubKey,
			},
		),
		rootTweak[:],
	)
	require.NoError(t, err)
	batchScript, err := txscript.PayToTaprootScript(rootKey)
	require.NoError(t, err)

	commitment := wire.NewMsgTx(3)
	commitment.AddTxIn(&wire.TxIn{
		PreviousOutPoint: intent.Outpoint,
	})
	commitment.AddTxOut(&wire.TxOut{
		Value:    int64(request.Amount),
		PkScript: batchScript,
	})
	commitment.AddTxOut(arkscript.AnchorOutput())
	packet, err := psbt.NewFromUnsignedTx(commitment)
	require.NoError(t, err)
	packet.Inputs[0] = psbt.PInput{
		WitnessUtxo: &wire.TxOut{
			Value:    int64(intent.ChainInfo.Amount),
			PkScript: intent.Address.Address.ScriptAddress(),
		},
	}

	sweepRoot := chainhash.HashH([]byte("sweep"))
	assetTree, err := tree.NewTree(
		wire.OutPoint{Hash: commitment.TxHash()}, commitment.TxOut[0],
		[]tree.LeafDescriptor{{
			Amount:      request.Amount,
			PkScript:    leafScript,
			CoSignerKey: request.SigningKey.PubKey,
		}}, h.operatorPubKey, sweepRoot[:], 2,
	)
	require.NoError(t, err)
	assetContext := tree.NewAssetTreeContext()
	assetContext.SetAssetRef(request.AssetRef)
	assetContext.SetNodeAssetAmount(assetTree.Root, request.AssetAmount)
	assetContext.SetSigningTweak(assetTree.Root.Input, rootTweak[:])
	assetContext.SetLeafAssetRoot(assetTree.Root.Input, assetRoot[:])
	assetTree.AssetContext = assetContext

	leafOutpoint, err := assetTree.Root.GetNonAnchorOutpoint()
	require.NoError(t, err)

	return &boundAssetVTXOFixture{
		intent:        intent,
		request:       request,
		commitmentTx:  packet,
		tree:          assetTree,
		leafOutpoint:  *leafOutpoint,
		sealedPackage: []byte("sealed package"),
	}
}

func TestCommitmentTxReceivedVerifiesAssetVTXO(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	fixture := newBoundAssetVTXOFixture(t, h)
	verifier := &recordingAssetVTXOVerifier{}
	registrar := &recordingOwnedScriptRegistrar{}
	h.env.AssetVTXOVerifier = verifier
	h.env.OwnedScriptRegistrar = registrar
	roundID := testRoundIDTr("asset-vtxo")
	h.withState(&CommitmentTxReceivedState{
		RoundID:      roundID,
		CommitmentTx: fixture.commitmentTx,
		TxID:         fixture.commitmentTx.UnsignedTx.TxHash(),
		SweepDelay:   1008,
		VTXOTreePaths: map[int]*tree.Tree{
			0: fixture.tree,
		},
		AssetLeafPackages: map[wire.OutPoint][]byte{
			fixture.leafOutpoint: fixture.sealedPackage,
		},
		Intents: Intents{
			Boarding: []BoardingIntent{fixture.intent},
			VTXOs:    []types.VTXORequest{fixture.request},
		},
		ClientTrees: make(map[SignerKey]*tree.Tree),
	})

	transition, err := h.sendEvent(&CommitmentTxBuilt{
		RoundID: roundID,
		Tx:      fixture.commitmentTx,
		VTXOTreePaths: map[int]*tree.Tree{
			0: fixture.tree,
		},
	})
	require.NoError(t, err)
	require.NotNil(t, transition)
	validated := assertStateType[*CommitmentTxValidatedState](h)
	require.Equal(t, 1, verifier.calls)
	require.Equal(t, fixture.request.AssetRef, verifier.assetRef)
	require.Equal(t, fixture.request.AssetAmount, verifier.assetAmount)
	require.Same(t, fixture.commitmentTx.UnsignedTx, verifier.commitmentTx)
	require.Equal(t, fixture.sealedPackage, verifier.sealedPackage)

	signerKey := NewSignerKey(fixture.request.SigningKey.PubKey)
	clientTree := validated.ClientTrees[signerKey]
	require.NotNil(t, clientTree)
	leaf := clientTree.Root.GetLeafNodes()[0]
	leafOutput, err := leafNonAnchorOutput(leaf)
	require.NoError(t, err)
	require.Equal(t, leafOutput.PkScript, registrar.pkScript)
	require.True(
		t, fixture.request.OwnerKey.PubKey.IsEqual(
			registrar.ownerKey.PubKey,
		),
	)
	plainScript, err := fixture.request.EffectivePkScript()
	require.NoError(t, err)
	require.NotEqual(t, plainScript, registrar.pkScript)
	require.Equal(
		t, fixture.sealedPackage,
		clientTree.AssetContext.SealedPackage(leaf.Input),
	)
	require.Empty(
		t, fixture.tree.AssetContext.SealedPackage(
			fixture.tree.Root.Input,
		),
	)

	vtxos, err := buildClientVTXOs(
		t.Context(), newMockOwnedScriptChecker(registrar.pkScript),
		Intents{
			VTXOs: []types.VTXORequest{fixture.request},
		}, map[SignerKey]*tree.Tree{
			signerKey: clientTree,
		},
		roundID,
	)
	require.NoError(t, err)
	require.Len(t, vtxos, 1)
	require.Equal(t, leafOutput.PkScript, vtxos[0].PkScript)
}

func TestValidateRequestedAssetVTXO(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	fixture := newBoundAssetVTXOFixture(t, h)

	clientTree, err := validateRequestedVTXO(
		fixture.tree, fixture.request, fixture.request.Amount,
		h.operatorPubKey,
	)
	require.NoError(t, err)
	require.NotNil(t, clientTree)

	fixture.tree.Root.Outputs[0].PkScript[2] ^= 1
	_, err = validateRequestedVTXO(
		fixture.tree, fixture.request, fixture.request.Amount,
		h.operatorPubKey,
	)
	require.ErrorContains(t, err, "VTXO script mismatch")
}

func TestValidateVTXOTreeBindingUsesAssetRoot(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	fixture := newBoundAssetVTXOFixture(t, h)
	trees := map[int]*tree.Tree{0: fixture.tree}
	require.NoError(
		t, validateVTXOTreeBinding(
			fixture.commitmentTx.UnsignedTx, trees,
		),
	)

	wrongRoot := chainhash.HashH([]byte("wrong batch asset root"))
	fixture.tree.AssetContext.SetSigningTweak(
		fixture.tree.Root.Input, wrongRoot[:],
	)
	require.ErrorContains(
		t, validateVTXOTreeBinding(
			fixture.commitmentTx.UnsignedTx, trees,
		),
		"recomputed tree root",
	)
}

func TestCommitmentTxReceivedRejectsUnverifiedAssetVTXO(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name      string
		configure func(*boardingTestHarness, *boundAssetVTXOFixture)
	}{
		{
			name: "missing verifier",
		},
		{
			name: "missing package",
			configure: func(h *boardingTestHarness,
				fixture *boundAssetVTXOFixture) {

				h.env.AssetVTXOVerifier =
					&recordingAssetVTXOVerifier{}
				fixture.sealedPackage = nil
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := newTestHarness(t)
			fixture := newBoundAssetVTXOFixture(t, h)
			if testCase.configure != nil {
				testCase.configure(h, fixture)
			}
			roundID := testRoundIDTr(testCase.name)
			h.withState(&CommitmentTxReceivedState{
				RoundID:      roundID,
				CommitmentTx: fixture.commitmentTx,
				TxID: fixture.commitmentTx.UnsignedTx.
					TxHash(),
				SweepDelay: 1008,
				VTXOTreePaths: map[int]*tree.Tree{
					0: fixture.tree,
				},
				AssetLeafPackages: map[wire.OutPoint][]byte{
					fixture.leafOutpoint: fixture.
						sealedPackage,
				},
				Intents: Intents{
					Boarding: []BoardingIntent{
						fixture.intent,
					},
					VTXOs: []types.VTXORequest{
						fixture.request,
					},
				},
				ClientTrees: make(map[SignerKey]*tree.Tree),
			})

			_, err := h.sendEvent(&CommitmentTxBuilt{
				RoundID: roundID,
				Tx:      fixture.commitmentTx,
				VTXOTreePaths: map[int]*tree.Tree{
					0: fixture.tree,
				},
			})
			require.NoError(t, err)
			failed := assertStateType[*ClientFailedState](h)
			require.Contains(
				t, failed.Reason,
				"asset VTXO verification failed",
			)
		})
	}
}
