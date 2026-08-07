package tapassets

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/stretchr/testify/require"
)

// fakeBatchDriver fabricates deterministic previews and commits consistent
// with the committer's fail-closed validation, with optional mutation
// hooks for tamper cases.
type fakeBatchDriver struct {
	mutateCommit func(*commitResult)
}

// batchMerkleRoot is the fake's deterministic composed root for one
// output.
func batchMerkleRoot(input wire.OutPoint, index uint32) tapsdk.Hash {
	seed := sha256.Sum256([]byte(fmt.Sprintf("batch/%s/%d", input, index)))

	return tapsdk.Hash(seed)
}

// batchAssetRoot is the fake's deterministic asset commitment root.
func batchAssetRoot(input wire.OutPoint, index uint32) tapsdk.Hash {
	seed := sha256.Sum256([]byte(fmt.Sprintf("asset/%s/%d", input, index)))

	return tapsdk.Hash(seed)
}

// Preview projects the deterministic roots for every requested output.
func (f *fakeBatchDriver) Preview(_ context.Context,
	request *tapsdk.CustomAnchorRequest, _ tapsdk.ConfirmedProofVerifier) (
	[]commitmentPreview, error) {

	template, err := parseAnchorTx(request.AnchorPSBT)
	if err != nil {
		return nil, err
	}
	input := template.TxIn[0].PreviousOutPoint

	previews := make([]commitmentPreview, len(request.Outputs))
	for i, out := range request.Outputs {
		previews[i] = commitmentPreview{
			logicalOutputID:   out.ID,
			anchorOutputIndex: out.AnchorOutputIndex,
			assetRoot: batchAssetRoot(
				input, out.AnchorOutputIndex,
			),
			merkleRoot: batchMerkleRoot(
				input, out.AnchorOutputIndex,
			),
		}
	}

	return previews, nil
}

// Commit echoes the request into a sealed-result projection with the same
// deterministic roots the preview reported.
func (f *fakeBatchDriver) Commit(_ context.Context,
	request *tapsdk.CustomAnchorRequest, _ tapsdk.ConfirmedProofVerifier) (
	*commitResult, error) {

	template, err := parseAnchorTx(request.AnchorPSBT)
	if err != nil {
		return nil, err
	}
	input := template.TxIn[0].PreviousOutPoint

	result := &commitResult{
		fundingMode: tapsdk.CustomAnchorFundingCallerFundedExact,
		anchorPSBT:  request.AnchorPSBT,
	}
	for _, out := range request.Outputs {
		result.outputs = append(result.outputs, commitOutput{
			anchorOutputIndex: out.AnchorOutputIndex,
			anchorOutpoint: sdkOutpoint(wire.OutPoint{
				Hash:  template.TxHash(),
				Index: out.AnchorOutputIndex,
			}),
			anchorValueSat: int64(out.AnchorValueSat),
			assetRef:       out.AssetRef,
			amount:         out.Amount,
			taprootMerkleRoot: batchMerkleRoot(
				input, out.AnchorOutputIndex,
			),
			taprootAssetRoot: batchAssetRoot(
				input, out.AnchorOutputIndex,
			),
			scriptMode: tapsdk.CustomAssetScriptOPTrue,
			opTrueWitness: [][]byte{
				{0x51},
			},
			proofBlob: []byte(fmt.Sprintf("batch-proof/%s", input)),
		})
	}
	result.packageBytes = []byte(fmt.Sprintf("batch-package/%s", input))

	if f.mutateCommit != nil {
		f.mutateCommit(result)
	}

	return result, nil
}

// DecodePackage is unused by the batch anchor committer.
func (f *fakeBatchDriver) DecodePackage([]byte) (*commitResult, error) {
	return nil, fmt.Errorf("decode is not used by the batch committer")
}

// parseAnchorTx extracts the unsigned transaction from a serialized PSBT.
func parseAnchorTx(anchorPSBT []byte) (*wire.MsgTx, error) {
	packet, err := psbtutil.Parse(anchorPSBT)
	if err != nil {
		return nil, err
	}

	return packet.UnsignedTx, nil
}

// newBatchAnchorFixture builds a request, its template transaction, and a
// committer over the fake driver.
func newBatchAnchorFixture(t *testing.T) (*BatchAnchorCommitter,
	*BatchAnchorRequest, *psbt.Packet) {

	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	userKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	anchorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	sweepLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		operatorKey.PubKey(), 1008,
	)
	require.NoError(t, err)

	fundingOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("funding-utxo")),
		Index: 1,
	}
	assetRef := tapsdk.AssetRefFromAssetID(
		tapsdk.AssetID(
			chainhash.HashH(
				[]byte("asset"),
			),
		),
	)

	req := &BatchAnchorRequest{
		AssetRef: assetRef,
		Amount:   1_500,
		Sources: []BatchAnchorSource{{
			ProofFile:         []byte("funding-proof"),
			Amount:            1_500,
			AnchorOutpoint:    fundingOutpoint,
			AnchorInternalKey: anchorKey.PubKey(),
		}},
		Cosigners: []*btcec.PublicKey{
			operatorKey.PubKey(), userKey.PubKey(),
		},
		SweepLeaf:      sweepLeaf,
		Digest:         tapsdk.Hash(chainhash.HashH([]byte("round"))),
		OutputIndex:    0,
		OutputValueSat: 30_000,
	}

	template := wire.NewMsgTx(2)
	template.AddTxIn(wire.NewTxIn(&fundingOutpoint, nil, nil))
	template.AddTxOut(&wire.TxOut{
		Value:    30_000,
		PkScript: []byte{0x51},
	})

	packet, err := psbt.NewFromUnsignedTx(template)
	require.NoError(t, err)

	return &BatchAnchorCommitter{driver: &fakeBatchDriver{}}, req, packet
}

// fundedFromTemplate simulates the funding wallet: extra fee input,
// change output, and the derived script patched onto the batch output.
func fundedFromTemplate(t *testing.T, template *psbt.Packet,
	derived *BatchAnchorScript) *psbt.Packet {

	t.Helper()

	fundedTx := template.UnsignedTx.Copy()
	fundedTx.TxOut[0].PkScript = append(
		[]byte(nil), derived.PkScript...,
	)
	fundedTx.AddTxIn(
		wire.NewTxIn(
			&wire.OutPoint{
				Hash:  chainhash.HashH([]byte("fee-input")),
				Index: 0,
			},
			nil,
			nil,
		),
	)
	fundedTx.AddTxOut(&wire.TxOut{
		Value:    5_000,
		PkScript: []byte{0x52},
	})

	funded, err := psbt.NewFromUnsignedTx(fundedTx)
	require.NoError(t, err)

	return funded
}

// TestBatchAnchorDeriveAndCommit proves the pre-funding derivation and the
// post-funding commit agree: the derived script reproduces from the
// committed roots and the root source carries the compact path binding.
func TestBatchAnchorDeriveAndCommit(t *testing.T) {
	t.Parallel()

	committer, req, template := newBatchAnchorFixture(t)
	ctx := context.Background()

	derived, err := committer.DeriveScript(ctx, req, template)
	require.NoError(t, err)

	// The derived script is the aggregate tweaked with the composed
	// root.
	wantInternal, err := tree.ComputeInternalKey(req.Cosigners)
	require.NoError(t, err)
	wantKey := txscript.ComputeTaprootOutputKey(
		wantInternal, derived.SigningTweak,
	)
	wantScript, err := txscript.PayToTaprootScript(wantKey)
	require.NoError(t, err)
	require.Equal(t, wantScript, derived.PkScript)

	funded := fundedFromTemplate(t, template, derived)

	commit, err := committer.Commit(ctx, req, funded, derived)
	require.NoError(t, err)
	require.NotEmpty(t, commit.PackageBytes)
	require.Equal(t, derived.PkScript, commit.Script.PkScript)
	require.Equal(
		t, derived.SigningTweak, commit.RootSource.SigningTweak,
	)
	require.Equal(t, derived.PkScript, commit.RootSource.BatchPkScript)

	// The root source chains through a one-step compact path bound to
	// the funded transaction.
	path := commit.RootSource.proofPath
	require.NotNil(t, path)
	require.Equal(t, req.Sources[0].ProofFile, path.ConfirmedBaseProof)
	require.Len(t, path.Steps, 1)

	require.Len(t, commit.RootSource.expectedSteps, 1)
	step := commit.RootSource.expectedSteps[0]
	require.Equal(
		t, sdkOutpoint(req.Sources[0].AnchorOutpoint),
		step.previousOutpoint,
	)
	require.Equal(
		t, serializeTx(funded.UnsignedTx), step.transaction,
	)
}

// TestBatchAnchorCommitFailClosed proves every divergence between the
// derivation, the funded transaction, and the committed result is
// rejected.
func TestBatchAnchorCommitFailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		corrupt func(req *BatchAnchorRequest, funded *psbt.Packet,
			derived *BatchAnchorScript, fake *fakeBatchDriver)
		wantErr string
	}{
		{
			name: "funded output misses derived script",
			corrupt: func(_ *BatchAnchorRequest,
				funded *psbt.Packet, _ *BatchAnchorScript,
				_ *fakeBatchDriver) {

				funded.UnsignedTx.TxOut[0].PkScript = []byte{
					0x53,
				}
			},
			wantErr: "does not carry the derived batch script",
		},
		{
			name: "committed merkle root diverges",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				_ *BatchAnchorScript, fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					r.outputs[0].taprootMerkleRoot[0] ^= 1
				}
			},
			wantErr: "merkle root diverged",
		},
		{
			name: "committed asset root diverges",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				_ *BatchAnchorScript, fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					r.outputs[0].taprootAssetRoot[0] ^= 1
				}
			},
			wantErr: "asset root diverged",
		},
		{
			name: "committed amount diverges",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				_ *BatchAnchorScript, fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					r.outputs[0].amount--
				}
			},
			wantErr: "committed amount",
		},
		{
			name: "committed value diverges",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				_ *BatchAnchorScript, fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					r.outputs[0].anchorValueSat++
				}
			},
			wantErr: "committed value",
		},
		{
			name: "committed transaction diverges",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				_ *BatchAnchorScript, fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					packet, err := psbtutil.Parse(
						r.anchorPSBT,
					)
					if err != nil {
						panic(err)
					}
					packet.UnsignedTx.LockTime++
					mutated, err := psbtutil.Serialize(
						packet,
					)
					if err != nil {
						panic(err)
					}
					r.anchorPSBT = mutated
				}
			},
			wantErr: "diverged from the funded transaction",
		},
		{
			name: "extra committed output",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				_ *BatchAnchorScript, fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					r.outputs = append(
						r.outputs, r.outputs[0],
					)
				}
			},
			wantErr: "want 1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			committer, req, template := newBatchAnchorFixture(t)
			fake, ok := committer.driver.(*fakeBatchDriver)
			require.True(t, ok)
			ctx := context.Background()

			derived, err := committer.DeriveScript(
				ctx, req, template,
			)
			require.NoError(t, err)

			funded := fundedFromTemplate(t, template, derived)
			test.corrupt(req, funded, derived, fake)

			_, err = committer.Commit(ctx, req, funded, derived)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

// TestBatchAnchorRequestGuards proves the request-level guards fire before
// any driver call.
func TestBatchAnchorRequestGuards(t *testing.T) {
	t.Parallel()

	committer, req, template := newBatchAnchorFixture(t)
	ctx := context.Background()

	// The template must spend the funding anchor.
	foreignTx := template.UnsignedTx.Copy()
	foreignTx.TxIn[0].PreviousOutPoint.Index++
	foreign, err := psbt.NewFromUnsignedTx(foreignTx)
	require.NoError(t, err)
	_, err = committer.DeriveScript(ctx, req, foreign)
	require.ErrorContains(t, err, "does not spend the funding anchor")

	// The funding proof is mandatory. The shallow copy shares the
	// sources slice, so the broken source is a fresh slice.
	broken := *req
	brokenSource := req.Sources[0]
	brokenSource.ProofFile = nil
	broken.Sources = []BatchAnchorSource{brokenSource}
	_, err = committer.DeriveScript(ctx, &broken, template)
	require.ErrorContains(t, err, "proof file 0 is required")

	// The asset amount is mandatory.
	broken = *req
	broken.Amount = 0
	_, err = committer.DeriveScript(ctx, &broken, template)
	require.ErrorContains(t, err, "asset amount is required")
}
