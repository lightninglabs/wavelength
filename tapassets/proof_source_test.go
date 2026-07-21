package tapassets

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestResolveCreatedAssetProofSource proves a sealed-package projection can be
// reconstructed byte-identically after restart without consulting tapd.
func TestResolveCreatedAssetProofSource(t *testing.T) {
	t.Parallel()

	committed, expectedRef, expectedRoot := proofSourceCommitResult(t)
	expectedOutpoint := committed.outputs[0].anchorOutpoint

	resolved, err := resolveCreatedAssetProofSource(
		committed, expectedOutpoint,
		committed.outputs[0].anchorValueSat, expectedRef,
		committed.outputs[0].amount, tapsdk.Hash(expectedRoot),
	)
	require.NoError(t, err)
	require.Equal(t, "input-0", resolved.LogicalInputID)
	require.Equal(t, "output-0", resolved.LogicalOutputID)
	require.Equal(t, AssetPacketActive, resolved.PacketRole)
	require.Equal(t, expectedRef.String(), resolved.AssetRef)
	require.Equal(t, uint64(100), resolved.AssetAmount)
	require.Equal(t, int64(330), resolved.CarrierValueSat)
	require.Equal(t, expectedRoot, resolved.TaprootAssetRoot)
	require.Equal(t, ProofSourceConfirmedFile,
		resolved.ProofSourceKind)
	require.Equal(
		t, committed.outputs[0].proofBlob, resolved.TransitionProof,
	)
	require.Equal(
		t, wire.TxWitness{{txscript.OP_TRUE}, {1, 2, 3}},
		resolved.OPTrueWitness,
	)

	var path tapsdk.AssetProofPath
	require.NoError(t, path.UnmarshalBinary(resolved.CompactProofPath))
	require.Len(t, path.Steps, 1)
	require.Equal(
		t, committed.outputs[0].proofBlob,
		path.Steps[0].TransitionProof,
	)

	// Mutating one result must not alter the package projection or the next
	// resolution performed by a fresh process instance.
	resolved.ProofSourceBlob[0] ^= 1
	resolved.TransitionProof[0] ^= 1
	resolved.CompactProofPath[0] ^= 1
	resolved.OPTrueWitness[0][0] ^= 1
	restarted, err := resolveCreatedAssetProofSource(
		cloneCommitResult(committed), expectedOutpoint,
		committed.outputs[0].anchorValueSat, expectedRef,
		committed.outputs[0].amount, tapsdk.Hash(expectedRoot),
	)
	require.NoError(t, err)
	require.Equal(
		t, committed.inputs[0].proofSource.blob,
		restarted.ProofSourceBlob,
	)
	require.Equal(
		t, committed.outputs[0].proofBlob, restarted.TransitionProof,
	)
	require.Equal(
		t, wire.TxWitness{{txscript.OP_TRUE}, {1, 2, 3}},
		restarted.OPTrueWitness,
	)
}

// TestResolveCreatedAssetProofSourceExtendsCompactPath verifies the same
// resolver appends to an existing compact source rather than replacing it.
func TestResolveCreatedAssetProofSourceExtendsCompactPath(t *testing.T) {
	t.Parallel()

	committed, expectedRef, expectedRoot := proofSourceCommitResult(t)
	base := committed.inputs[0].proofSource.blob
	path := &tapsdk.AssetProofPath{
		Version:            tapsdk.AssetProofPathVersionV0,
		ConfirmedBaseProof: append([]byte(nil), base...),
	}
	encoded, err := path.MarshalBinary()
	require.NoError(t, err)
	committed.inputs[0].proofSource.kind =
		tapsdk.CustomAnchorProofSourceCompactPath
	committed.inputs[0].proofSource.blob = encoded

	resolved, err := resolveCreatedAssetProofSource(
		committed, committed.outputs[0].anchorOutpoint,
		committed.outputs[0].anchorValueSat, expectedRef,
		committed.outputs[0].amount, tapsdk.Hash(expectedRoot),
	)
	require.NoError(t, err)
	require.Equal(t, ProofSourceCompactPath, resolved.ProofSourceKind)
	var extended tapsdk.AssetProofPath
	require.NoError(
		t, extended.UnmarshalBinary(
			resolved.CompactProofPath,
		),
	)
	require.Equal(t, base, extended.ConfirmedBaseProof)
	require.Len(t, extended.Steps, 1)
}

// TestResolveCreatedAssetProofSourceRejectsMismatches pins the output,
// predecessor, script-mode, and compact-path ambiguity checks.
func TestResolveCreatedAssetProofSourceRejectsMismatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*commitResult, *tapsdk.Outpoint, *int64,
			*tapsdk.AssetRef, *uint64, *chainhash.Hash)
		wantErr string
	}{
		{
			name: "outpoint",
			mutate: func(_ *commitResult, outpoint *tapsdk.Outpoint,
				_ *int64, _ *tapsdk.AssetRef, _ *uint64,
				_ *chainhash.Hash) {

				outpoint.Index++
			},
			wantErr: "does not create",
		},
		{
			name: "carrier value",
			mutate: func(_ *commitResult, _ *tapsdk.Outpoint,
				carrier *int64, _ *tapsdk.AssetRef, _ *uint64,
				_ *chainhash.Hash) {

				(*carrier)++
			},
			wantErr: "does not create",
		},
		{
			name: "asset ref",
			mutate: func(_ *commitResult, _ *tapsdk.Outpoint,
				_ *int64, ref *tapsdk.AssetRef, _ *uint64,
				_ *chainhash.Hash) {

				*ref = tapsdk.AssetRefFromAssetID(
					tapsdk.AssetID{99},
				)
			},
			wantErr: "does not create",
		},
		{
			name: "amount",
			mutate: func(_ *commitResult, _ *tapsdk.Outpoint,
				_ *int64, _ *tapsdk.AssetRef, amount *uint64,
				_ *chainhash.Hash) {

				(*amount)++
			},
			wantErr: "does not create",
		},
		{
			name: "root",
			mutate: func(_ *commitResult, _ *tapsdk.Outpoint,
				_ *int64, _ *tapsdk.AssetRef, _ *uint64,
				root *chainhash.Hash) {

				root[0] ^= 1
			},
			wantErr: "does not create",
		},
		{
			name: "non op true",
			mutate: func(result *commitResult, _ *tapsdk.Outpoint,
				_ *int64, _ *tapsdk.AssetRef, _ *uint64,
				_ *chainhash.Hash) {

				result.outputs[0].scriptMode =
					tapsdk.CustomAssetScriptExternal
			},
			wantErr: "not spendable through OP_TRUE",
		},
		{
			name: "missing predecessor",
			mutate: func(result *commitResult, _ *tapsdk.Outpoint,
				_ *int64, _ *tapsdk.AssetRef, _ *uint64,
				_ *chainhash.Hash) {

				result.inputs = nil
			},
			wantErr: "predecessor is not present",
		},
		{
			name: "ambiguous predecessor",
			mutate: func(result *commitResult, _ *tapsdk.Outpoint,
				_ *int64, _ *tapsdk.AssetRef, _ *uint64,
				_ *chainhash.Hash) {

				result.inputs = append(
					result.inputs, result.inputs[0],
				)
			},
			wantErr: "multiple possible predecessor",
		},
		{
			name: "malformed transition",
			mutate: func(result *commitResult, _ *tapsdk.Outpoint,
				_ *int64, _ *tapsdk.AssetRef, _ *uint64,
				_ *chainhash.Hash) {

				result.outputs[0].proofBlob = []byte(
					"bad-proof",
				)
			},
			wantErr: "summarize created asset proof",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			committed, ref, root := proofSourceCommitResult(t)
			outpoint := committed.outputs[0].anchorOutpoint
			carrier := committed.outputs[0].anchorValueSat
			amount := committed.outputs[0].amount
			test.mutate(
				committed, &outpoint, &carrier, &ref, &amount,
				&root,
			)
			_, err := resolveCreatedAssetProofSource(
				committed, outpoint, carrier, ref, amount,
				tapsdk.Hash(root),
			)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

// TestResolveCreatedAssetProofSourceRejectsDepthExhaustion proves restart
// reconstruction cannot grow a compact path beyond the SDK's bounded depth.
func TestResolveCreatedAssetProofSourceRejectsDepthExhaustion(t *testing.T) {
	t.Parallel()

	committed, ref, root := proofSourceCommitResult(t)
	path := &tapsdk.AssetProofPath{
		Version: tapsdk.AssetProofPathVersionV0,
		ConfirmedBaseProof: append(
			[]byte(nil), committed.inputs[0].proofSource.blob...,
		),
		Steps: make(
			[]tapsdk.AssetProofPathStep,
			tapsdk.AssetProofPathMaxDepth,
		),
	}
	for idx := range path.Steps {
		path.Steps[idx].TransitionProof = append(
			[]byte(nil), committed.outputs[0].proofBlob...,
		)
	}
	encoded, err := path.MarshalBinary()
	require.NoError(t, err)
	committed.inputs[0].proofSource.kind =
		tapsdk.CustomAnchorProofSourceCompactPath
	committed.inputs[0].proofSource.blob = encoded

	_, err = resolveCreatedAssetProofSource(
		committed, committed.outputs[0].anchorOutpoint,
		committed.outputs[0].anchorValueSat, ref,
		committed.outputs[0].amount, tapsdk.Hash(root),
	)
	require.ErrorContains(t, err, "path depth")
}

func proofSourceCommitResult(t *testing.T) (*commitResult, tapsdk.AssetRef,
	chainhash.Hash) {

	t.Helper()
	baseFile, baseProof, senderKey := proofSourceBase(t)
	transition := proofSourceTransition(
		t, baseProof, senderKey, testPrivateKey(t, 31),
	)
	transitionBytes, err := transition.Bytes()
	require.NoError(t, err)
	step := tapsdk.AssetProofPathStep{
		TransitionProof: transitionBytes,
	}
	summary, err := step.Summary()
	require.NoError(t, err)
	root := chainhash.Hash{9, 8, 7}

	return &commitResult{
		inputs: []commitInput{{
			logicalInputID:    "input-0",
			logicalInputIndex: 3,
			packetIndex:       1,
			packetRole:        tapsdk.CustomAnchorPacketRoleActive,
			virtualInputIndex: 2,
			anchorInputIndex:  0,
			anchorOutpoint:    summary.PreviousAnchorOutpoint,
			assetRef:          summary.AssetRef,
			issuanceID:        summary.IssuanceID,
			amount:            baseProof.Asset.Amount,
			proofSource: commitProofSource{
				kind: tapsdk.
					CustomAnchorProofSourceConfirmedFile,
				blob: append([]byte(nil), baseFile...),
			},
		}},
		outputs: []commitOutput{{
			logicalOutputID:    "output-0",
			logicalOutputIndex: 4,
			packetIndex:        1,
			packetRole:         tapsdk.CustomAnchorPacketRoleActive,
			virtualOutputIndex: 5,
			anchorOutputIndex:  summary.AnchorOutpoint.Index,
			anchorOutpoint:     summary.AnchorOutpoint,
			anchorValueSat:     summary.AnchorValueSat,
			assetRef:           summary.AssetRef,
			issuanceID:         summary.IssuanceID,
			amount:             summary.Amount,
			taprootAssetRoot:   tapsdk.Hash(root),
			scriptKey:          summary.ScriptKey,
			scriptMode:         tapsdk.CustomAssetScriptOPTrue,
			opTrueWitness: [][]byte{
				{
					txscript.OP_TRUE,
				}, {
					1,
					2,
					3,
				},
			},
			proofBlob: append([]byte(nil), transitionBytes...),
		}},
	}, summary.AssetRef, root
}

func proofSourceBase(t *testing.T) ([]byte, *proof.Proof, *btcec.PrivateKey) {
	t.Helper()
	senderKey := testPrivateKey(t, 29)
	internalKey := testPrivateKey(t, 30)
	genesis := asset.Genesis{
		FirstPrevOut: wire.OutPoint{
			Hash: chainhash.Hash{
				1,
			},
			Index: 1,
		},
		Tag:         "wavelength-proof-source",
		OutputIndex: 0,
		Type:        asset.Normal,
	}
	amount := uint64(100)
	version := commitment.TapCommitmentV2
	tapCommitment, assets, err := commitment.Mint(
		&version, genesis, nil, &commitment.AssetDetails{
			Version: asset.V1,
			Type:    asset.Normal,
			ScriptKey: keychain.KeyDescriptor{
				PubKey: senderKey.PubKey(),
			},
			Amount: &amount,
		},
	)
	require.NoError(t, err)
	anchorTx := proofSourceAnchorTx(
		t, genesis.FirstPrevOut, internalKey.PubKey(), tapCommitment,
	)
	proofs, err := proof.NewMintingBlobs(
		&proof.MintParams{
			BaseProofParams: proof.BaseProofParams{
				Block:            proofSourceBlock(anchorTx),
				BlockHeight:      100,
				Tx:               anchorTx,
				TxIndex:          0,
				OutputIndex:      0,
				InternalKey:      internalKey.PubKey(),
				TaprootAssetRoot: tapCommitment,
			},
			GenesisPoint: genesis.FirstPrevOut,
		}, proof.MockVerifierCtx,
		proof.WithGenOption(proof.WithVersion(proof.TransitionV1)),
	)
	require.NoError(t, err)
	baseProof := proofs[asset.ToSerialized(assets[0].ScriptKey.PubKey)]
	require.NotNil(t, baseProof)
	baseFile, err := proof.EncodeAsProofFile(baseProof)
	require.NoError(t, err)

	return baseFile, baseProof, senderKey
}

func proofSourceTransition(t *testing.T, previous *proof.Proof,
	spendKey, recipientKey *btcec.PrivateKey) *proof.Proof {

	t.Helper()
	newAsset := previous.Asset.Copy()
	newAsset.ScriptKey = asset.NewScriptKeyBip86(keychain.KeyDescriptor{
		PubKey: recipientKey.PubKey(),
	})
	previousID := &asset.PrevID{
		OutPoint: previous.OutPoint(),
		ID:       previous.Asset.ID(),
		ScriptKey: asset.ToSerialized(
			previous.Asset.ScriptKey.PubKey,
		),
	}
	newAsset.PrevWitnesses = []asset.Witness{{PrevID: previousID}}
	inputs := commitment.InputSet{*previousID: &previous.Asset}
	virtualTx, _, err := tapscript.VirtualTx(newAsset, inputs)
	require.NoError(t, err)
	virtualTx = asset.VirtualTxWithInput(virtualTx, 0, 0, 0, nil)
	sigHash, err := tapscript.InputKeySpendSigHash(
		virtualTx, &previous.Asset, newAsset, 0,
		txscript.SigHashDefault,
	)
	require.NoError(t, err)
	tweakedSpendKey := txscript.TweakTaprootPrivKey(*spendKey, nil)
	signingKey := spendKey
	if bytes.Equal(
		schnorr.SerializePubKey(
			tweakedSpendKey.PubKey(),
		),
		schnorr.SerializePubKey(previous.Asset.ScriptKey.PubKey),
	) {

		signingKey = tweakedSpendKey
	}
	signature, err := schnorr.Sign(signingKey, sigHash)
	require.NoError(t, err)
	newAsset.PrevWitnesses[0].TxWitness = wire.TxWitness{
		signature.Serialize(),
	}

	assetCommitment, err := commitment.NewAssetCommitment(newAsset)
	require.NoError(t, err)
	version := commitment.TapCommitmentV2
	tapCommitment, err := commitment.NewTapCommitment(
		&version, assetCommitment,
	)
	require.NoError(t, err)
	spentAsset, err := asset.MakeSpentAsset(newAsset.PrevWitnesses[0])
	require.NoError(t, err)
	require.NoError(
		t,
		tapCommitment.MergeAltLeaves(
			asset.ToAltLeaves(
				[]*asset.Asset{spentAsset},
			),
		),
	)
	anchorTx := proofSourceAnchorTx(
		t, previous.OutPoint(), recipientKey.PubKey(), tapCommitment,
	)
	transition, err := proof.CreateTransitionProof(
		previous.OutPoint(), &proof.TransitionParams{
			BaseProofParams: proof.BaseProofParams{
				Block:            proofSourceBlock(anchorTx),
				Tx:               anchorTx,
				TxIndex:          0,
				OutputIndex:      0,
				InternalKey:      recipientKey.PubKey(),
				TaprootAssetRoot: tapCommitment,
			},
			NewAsset: newAsset,
		}, proof.WithVersion(proof.TransitionV1),
	)
	require.NoError(t, err)

	return transition
}

func proofSourceAnchorTx(t *testing.T, previous wire.OutPoint,
	internalKey *btcec.PublicKey,
	tapCommitment *commitment.TapCommitment) *wire.MsgTx {

	t.Helper()
	root := tapCommitment.TapscriptRoot(nil)
	outputKey := txscript.ComputeTaprootOutputKey(internalKey, root[:])
	pkScript, err := txscript.PayToTaprootScript(outputKey)
	require.NoError(t, err)

	return &wire.MsgTx{
		Version: 3,
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: previous,
		}},
		TxOut: []*wire.TxOut{{
			Value:    330,
			PkScript: pkScript,
		}},
	}
}

func proofSourceBlock(anchorTx *wire.MsgTx) *wire.MsgBlock {
	tree := blockchain.BuildMerkleTreeStore(
		[]*btcutil.Tx{btcutil.NewTx(anchorTx)}, false,
	)

	return &wire.MsgBlock{
		Header: wire.BlockHeader{
			MerkleRoot: *tree[len(tree)-1],
		},
		Transactions: []*wire.MsgTx{
			anchorTx,
		},
	}
}
