package tapassets

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/tap-sdk/customanchor"
	"github.com/stretchr/testify/require"
)

type acceptAllProofChainVerifier struct{}

func (acceptAllProofChainVerifier) VerifyProofChain(_ context.Context,
	_ []byte) error {

	return nil
}

// TestNewOORProofVerifierAllowsLineageAndPackages verifies that the OOR proof
// verifier accepts commitment, tree, checkpoint, and Ark anchor transactions
// from the supplied lineage and OOR packages while rejecting unknown
// transactions and unknown anchor outputs.
func TestNewOORProofVerifierAllowsLineageAndPackages(t *testing.T) {
	t.Parallel()

	f := newAssetVTXOFixture(t)

	cpTx := wire.NewMsgTx(3)
	cpTx.AddTxIn(wire.NewTxIn(&f.leafOutpoint, nil, nil))
	cpTx.AddTxOut(wire.NewTxOut(1_000, []byte{0x51, 0x20, 0x01}))

	cpOP := wire.OutPoint{Hash: cpTx.TxHash(), Index: 0}
	arkTx := wire.NewMsgTx(3)
	arkTx.AddTxIn(wire.NewTxIn(&cpOP, nil, nil))
	arkTx.AddTxOut(wire.NewTxOut(1_000, []byte{0x51, 0x20, 0x02}))

	verifier, err := NewOORProofVerifier(
		acceptAllProofChainVerifier{},
		[]OORLineageAnchor{{
			CommitmentTxID: f.commitmentTx.TxHash(),
			TreePath:       f.path,
		}},
		[]OORPackageTxs{{
			ArkTx:         arkTx,
			CheckpointTxs: []*wire.MsgTx{cpTx},
		}},
	)
	require.NoError(t, err)
	unconfVerifier, ok := verifier.(customanchor.UnconfirmedAnchorVerifier)
	require.True(t, ok)

	serializeTx := func(tx *wire.MsgTx) []byte {
		var buf bytes.Buffer
		require.NoError(t, tx.Serialize(&buf))

		return buf.Bytes()
	}

	// Commitment batch output, leaf output, checkpoint output, and Ark
	// output are all accepted.
	rootAnchor := customanchor.UnconfirmedAnchorVerification{
		StepIndex:         0,
		AnchorTransaction: serializeTx(f.commitmentTx),
		AnchorOutpoint:    sdkOutpoint(f.path.Root.Input),
	}
	require.NoError(
		t,
		unconfVerifier.VerifyUnconfirmedAnchor(
			t.Context(), rootAnchor,
		),
	)
	require.NoError(
		t,
		unconfVerifier.VerifyUnconfirmedAnchor(
			t.Context(),
			customanchor.UnconfirmedAnchorVerification{
				StepIndex:         1,
				AnchorTransaction: serializeTx(cpTx),
				AnchorOutpoint:    sdkOutpoint(cpOP),
			},
		),
	)
	require.NoError(
		t,
		unconfVerifier.VerifyUnconfirmedAnchor(
			t.Context(),
			customanchor.UnconfirmedAnchorVerification{
				StepIndex:         2,
				AnchorTransaction: serializeTx(arkTx),
				AnchorOutpoint: sdkOutpoint(wire.OutPoint{
					Hash:  arkTx.TxHash(),
					Index: 0,
				}),
			},
		),
	)

	// An unknown transaction is rejected.
	rogueTx := wire.NewMsgTx(3)
	rogueTx.AddTxIn(
		wire.NewTxIn(
			&wire.OutPoint{
				Hash: chainhash.Hash{0x99},
			},
			nil,
			nil,
		),
	)
	rogueTx.AddTxOut(wire.NewTxOut(1_000, []byte{0x51, 0x20, 0x09}))
	require.ErrorContains(
		t,
		unconfVerifier.VerifyUnconfirmedAnchor(
			t.Context(),
			customanchor.UnconfirmedAnchorVerification{
				StepIndex:         2,
				AnchorTransaction: serializeTx(rogueTx),
				AnchorOutpoint: sdkOutpoint(wire.OutPoint{
					Hash:  rogueTx.TxHash(),
					Index: 0,
				}),
			},
		),
		"not in the VTXO lineage",
	)

	// A mismatched outpoint index on an allowed transaction is rejected.
	require.ErrorContains(
		t,
		unconfVerifier.VerifyUnconfirmedAnchor(
			t.Context(),
			customanchor.UnconfirmedAnchorVerification{
				StepIndex:         2,
				AnchorTransaction: serializeTx(arkTx),
				AnchorOutpoint: sdkOutpoint(wire.OutPoint{
					Hash:  arkTx.TxHash(),
					Index: 7,
				}),
			},
		),
		"not on the VTXO path",
	)
}

// TestVerifyOORAssetVTXORejectsInvalidInputs checks fail-closed validation on
// VerifyOORAssetVTXO before proof verification.
func TestVerifyOORAssetVTXORejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	v, err := NewVTXOVerifier(acceptAllProofChainVerifier{})
	require.NoError(t, err)

	arkTx := wire.NewMsgTx(3)
	arkTx.AddTxOut(wire.NewTxOut(1_000, []byte{0x51, 0x20, 0x02}))
	arkOP := wire.OutPoint{Hash: arkTx.TxHash(), Index: 0}
	root := chainhash.Hash{0xaa}
	testRef := tapsdk.AssetRef(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" +
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	).String()

	// Empty sealed package rejected.
	err = v.VerifyOORAssetVTXO(t.Context(), VerifyOORAssetVTXORequest{
		AssetRef:    testRef,
		AssetAmount: 100,
		AssetRoot:   root,
		Outpoint:    arkOP,
		ArkTx:       arkTx,
	})
	require.ErrorContains(t, err, "sealed ark asset package is required")

	// Mismatched outpoint hash rejected.
	err = v.VerifyOORAssetVTXO(
		t.Context(), VerifyOORAssetVTXORequest{
			AssetRef:    testRef,
			AssetAmount: 100,
			AssetRoot:   root,
			Outpoint: wire.OutPoint{
				Hash:  chainhash.Hash{0x01},
				Index: 0,
			},
			ArkTx:            arkTx,
			SealedArkPackage: []byte("non-empty"),
		},
	)
	require.ErrorContains(t, err, "does not match ark tx")

	// Out-of-bounds outpoint index rejected.
	err = v.VerifyOORAssetVTXO(t.Context(), VerifyOORAssetVTXORequest{
		AssetRef:         testRef,
		AssetAmount:      100,
		AssetRoot:        root,
		Outpoint:         wire.OutPoint{Hash: arkTx.TxHash(), Index: 3},
		ArkTx:            arkTx,
		SealedArkPackage: []byte("non-empty"),
	})
	require.ErrorContains(t, err, "out of bounds")
}
