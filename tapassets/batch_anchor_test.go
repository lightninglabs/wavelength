package tapassets

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
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

type fakeBatchDriver struct {
	mutateCommit func(*commitResult)
	commitErr    error
	afterCommit  func()
	commits      int
	packages     map[string]*commitResult
	paths        []*tapsdk.AssetProofPath
	publishErr   error
	published    int
}

func (f *fakeBatchDriver) Publish(_ context.Context, _, _ []byte) error {
	f.published++

	return f.publishErr
}

// batchMerkleRoot returns a stable root for a test output.
func batchMerkleRoot(input wire.OutPoint, index uint32) tapsdk.Hash {
	seed := sha256.Sum256([]byte(fmt.Sprintf("batch/%s/%d", input, index)))

	return tapsdk.Hash(seed)
}

// batchAssetRoot returns a stable asset root for a test output.
func batchAssetRoot(input wire.OutPoint, index uint32) tapsdk.Hash {
	seed := sha256.Sum256([]byte(fmt.Sprintf("asset/%s/%d", input, index)))

	return tapsdk.Hash(seed)
}

func (f *fakeBatchDriver) Preview(_ context.Context,
	request *tapsdk.CustomAnchorRequest, _ tapsdk.ConfirmedProofVerifier) (
	[]outputCommitmentPreview, error) {

	template, err := parseAnchorTx(request.AnchorPSBT)
	if err != nil {
		return nil, err
	}
	input := template.TxIn[0].PreviousOutPoint

	previews := make(
		[]outputCommitmentPreview, 0,
		len(request.Outputs)*len(request.Inputs),
	)
	for _, out := range request.Outputs {
		for range request.Inputs {
			previews = append(previews, outputCommitmentPreview{
				logicalOutputID:   out.ID,
				anchorOutputIndex: out.AnchorOutputIndex,
				assetRoot: batchAssetRoot(
					input, out.AnchorOutputIndex,
				),
				merkleRoot: batchMerkleRoot(
					input, out.AnchorOutputIndex,
				),
			})
		}
	}

	return previews, nil
}

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
	remaining := make([]uint64, len(request.Inputs))
	issuances := make([]tapsdk.AssetID, len(request.Inputs))
	for idx, input := range request.Inputs {
		anchorIndex := idx
		if anchorIndex >= len(template.TxIn) {
			return nil, fmt.Errorf("fake request input exceeds " +
				"anchor inputs")
		}
		issuance := tapsdk.AssetID(sha256.Sum256([]byte(input.ID)))
		issuances[idx] = issuance
		remaining[idx] = input.Amount
		result.inputs = append(result.inputs, commitInput{
			logicalInputID:    input.ID,
			packetIndex:       uint32(idx),
			packetRole:        tapsdk.CustomAnchorPacketRoleActive,
			virtualInputIndex: 0,
			anchorOutpoint: sdkOutpoint(
				template.TxIn[anchorIndex].PreviousOutPoint,
			),
			assetRef:   input.AssetRef,
			issuanceID: issuance,
			amount:     input.Amount,
			proofSource: commitProofSource{
				kind: tapsdk.
					CustomAnchorProofSourceConfirmedFile,
				blob: append([]byte(nil), input.ProofFile...),
			},
		})
	}
	for _, out := range request.Outputs {
		scriptMode := tapsdk.CustomAssetScriptOPTrue
		if out.ID == batchAnchorChangeID {
			scriptMode = scriptExternal
		}
		amount := out.Amount
		for inputIdx := range remaining {
			if amount == 0 {
				break
			}
			allocated := min(amount, remaining[inputIdx])
			if allocated == 0 {
				continue
			}
			amount -= allocated
			remaining[inputIdx] -= allocated
			result.outputs = append(result.outputs, commitOutput{
				logicalOutputID: out.ID,
				packetIndex:     uint32(inputIdx),
				packetRole: tapsdk.
					CustomAnchorPacketRoleActive,
				anchorOutputIndex: out.AnchorOutputIndex,
				anchorOutpoint: sdkOutpoint(wire.OutPoint{
					Hash:  template.TxHash(),
					Index: out.AnchorOutputIndex,
				}),
				anchorValueSat: int64(out.AnchorValueSat),
				assetRef:       out.AssetRef,
				issuanceID:     issuances[inputIdx],
				amount:         allocated,
				taprootMerkleRoot: batchMerkleRoot(
					input, out.AnchorOutputIndex,
				),
				taprootAssetRoot: batchAssetRoot(
					input, out.AnchorOutputIndex,
				),
				scriptMode: scriptMode,
				opTrueWitness: [][]byte{
					{0x51},
				},
				proofBlob: []byte(
					fmt.Sprintf("batch-proof/%s/%d", input,
						inputIdx),
				),
			})
		}
		if amount != 0 {
			return nil, fmt.Errorf("fake output exceeds input " +
				"amount")
		}
	}
	result.packageBytes = []byte(fmt.Sprintf("batch-package/%s", input))

	if f.mutateCommit != nil {
		f.mutateCommit(result)
	}
	f.commits++
	if f.afterCommit != nil {
		f.afterCommit()
	}
	if f.commitErr != nil {
		return nil, f.commitErr
	}
	if f.packages == nil {
		f.packages = make(map[string]*commitResult)
	}
	f.packages[string(result.packageBytes)] = result

	return result, nil
}

// DecodePackage restores a prior fake commit for journal replay.
func (f *fakeBatchDriver) DecodePackage(encoded []byte) (*commitResult, error) {
	result := f.packages[string(encoded)]
	if result == nil {
		return nil, fmt.Errorf("fake package not found")
	}

	return result, nil
}

// parseAnchorTx extracts the unsigned transaction from a serialized PSBT.
func parseAnchorTx(anchorPSBT []byte) (*wire.MsgTx, error) {
	packet, err := psbtutil.Parse(anchorPSBT)
	if err != nil {
		return nil, err
	}

	return packet.UnsignedTx, nil
}

// newBatchAnchorFixture creates a single-source batch request.
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
			Verifier:          staticProofVerifier{},
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

	fake := &fakeBatchDriver{}
	committer := &BatchAnchorCommitter{
		driver: fake,
		store:  newMemoryStore(),
		encodeProofPath: func(path *tapsdk.AssetProofPath) ([]byte,
			error) {

			fake.paths = append(fake.paths, path.Clone())

			encoded := fmt.Sprintf("proof-path-%d", len(fake.paths))

			return []byte(encoded), nil
		},
	}

	return committer, req, packet
}

// fundedFromTemplate adds Bitcoin funding and change.
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

// TestBatchAnchorDeriveAndCommit checks the complete batch-anchor flow.
func TestBatchAnchorDeriveAndCommit(t *testing.T) {
	t.Parallel()

	committer, req, template := newBatchAnchorFixture(t)
	ctx := context.Background()

	derived, err := committer.DeriveScript(ctx, req, template)
	require.NoError(t, err)

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

	require.Len(t, commit.RootSource.Inputs, 1)
	rootInput := commit.RootSource.Inputs[0]
	fake, ok := committer.driver.(*fakeBatchDriver)
	require.True(t, ok)
	require.Len(t, fake.paths, 1)
	path := fake.paths[0]
	require.Equal(t, req.Sources[0].ProofFile, path.ConfirmedBaseProof)
	require.Len(t, path.Steps, 1)

	verifier, ok := rootInput.Verifier.(*treePathVerifier)
	require.True(t, ok)
	require.Len(t, verifier.steps, 1)
	step := verifier.steps[0]
	require.Equal(
		t, sdkOutpoint(req.Sources[0].AnchorOutpoint),
		step.previousOutpoint,
	)
	require.Equal(
		t, serializeTx(funded.UnsignedTx), step.transaction,
	)
}

func TestBatchAnchorCosigners(t *testing.T) {
	t.Parallel()

	committer, req, template := newBatchAnchorFixture(t)
	req.Cosigners = append(req.Cosigners, req.Cosigners[1])

	derived, err := committer.DeriveScript(t.Context(), req, template)
	require.NoError(t, err)

	want, err := tree.ComputeInternalKey(req.Cosigners[:2])
	require.NoError(t, err)
	require.True(t, derived.InternalKey.IsEqual(want))

	req.Cosigners[1] = nil
	_, err = committer.DeriveScript(t.Context(), req, template)
	require.ErrorContains(t, err, "batch cosigner 1 is required")
}

// TestBatchAnchorCommitValidation checks mismatched commit data.
func TestBatchAnchorCommitValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		corrupt func(req *BatchAnchorRequest, funded *psbt.Packet,
			derived *BatchAnchorScript, fake *fakeBatchDriver)
		wantErr string
	}{
		{
			name: "committed funding mode diverges",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				_ *BatchAnchorScript, fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					r.fundingMode = tapsdk.
						CustomAnchorFundingWalletFunded
				}
			},
			wantErr: "unexpected funding mode",
		},
		{
			name: "committed input outpoint diverges",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				_ *BatchAnchorScript, fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					r.inputs[0].anchorOutpoint.Index++
				}
			},
			wantErr: "input 0 outpoint mismatch",
		},
		{
			name: "committed input asset diverges",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				_ *BatchAnchorScript, fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					r.inputs[0].assetRef =
						tapsdk.AssetRefFromAssetID(
							tapsdk.AssetID{0xff},
						)
				}
			},
			wantErr: "input 0 asset ref mismatch",
		},
		{
			name: "committed input amount diverges",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				_ *BatchAnchorScript, fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					r.inputs[0].amount--
				}
			},
			wantErr: "input 0 amount",
		},
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
			name: "committed asset ref diverges",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				_ *BatchAnchorScript, fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					r.outputs[0].assetRef =
						tapsdk.AssetRefFromAssetID(
							tapsdk.AssetID{0xff},
						)
				}
			},
			wantErr: "asset ref mismatch",
		},
		{
			name: "committed script mode diverges",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				_ *BatchAnchorScript, fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					r.outputs[0].scriptMode = scriptExternal
				}
			},
			wantErr: "not OP_TRUE",
		},
		{
			name: "committed outpoint diverges",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				_ *BatchAnchorScript, fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					r.outputs[0].anchorOutpoint.Index++
				}
			},
			wantErr: "committed outpoint mismatch",
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
			wantErr: "repeats issuance",
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

// addBatchAnchorChange adds a 600-unit asset change output.
func addBatchAnchorChange(t *testing.T, req *BatchAnchorRequest,
	template *psbt.Packet) *psbt.Packet {

	t.Helper()

	scriptPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	scriptPub, err := tapsdk.ParsePubKey(
		scriptPriv.PubKey().SerializeCompressed(),
	)
	require.NoError(t, err)
	internalPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	internalPub, err := tapsdk.ParsePubKey(
		internalPriv.PubKey().SerializeCompressed(),
	)
	require.NoError(t, err)

	req.Amount = 900
	req.Change = &BatchAnchorChange{
		Amount:         600,
		OutputIndex:    1,
		OutputValueSat: 1_000,
		ScriptKey: tapsdk.ScriptKey{
			PubKey: scriptPub,
		},
		AnchorInternalKey: tapsdk.InternalKey{
			PubKey: internalPub,
		},
	}

	tx := template.UnsignedTx.Copy()
	tx.AddTxOut(&wire.TxOut{
		Value:    req.Change.OutputValueSat,
		PkScript: []byte{0x51},
	})
	rebuilt, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	return rebuilt
}

// TestBatchAnchorChangeDeriveAndCommit checks asset change handling.
func TestBatchAnchorChangeDeriveAndCommit(t *testing.T) {
	t.Parallel()

	committer, req, template := newBatchAnchorFixture(t)
	template = addBatchAnchorChange(t, req, template)
	ctx := context.Background()

	derived, err := committer.DeriveScript(ctx, req, template)
	require.NoError(t, err)
	require.NotEmpty(t, derived.ChangePkScript)

	input := template.UnsignedTx.TxIn[0].PreviousOutPoint
	changeInternal, err := btcec.ParsePubKey(
		req.Change.AnchorInternalKey.PubKey[:],
	)
	require.NoError(t, err)
	wantScript, err := composedScript(
		changeInternal, batchMerkleRoot(input, req.Change.OutputIndex),
	)
	require.NoError(t, err)
	require.Equal(t, wantScript, derived.ChangePkScript)

	funded := fundedFromTemplate(t, template, derived)
	funded.UnsignedTx.TxOut[1].PkScript = append(
		[]byte(nil), derived.ChangePkScript...,
	)

	commit, err := committer.Commit(ctx, req, funded, derived)
	require.NoError(t, err)
	require.Equal(t, derived.ChangePkScript, commit.Script.ChangePkScript)
}

// TestBatchAnchorChangeValidation checks mismatched change data.
func TestBatchAnchorChangeValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		corrupt func(req *BatchAnchorRequest, funded *psbt.Packet,
			fake *fakeBatchDriver)
		wantErr string
	}{
		{
			name: "sources do not cover batch and change",
			corrupt: func(req *BatchAnchorRequest, _ *psbt.Packet,
				_ *fakeBatchDriver) {

				req.Change.Amount--
			},
			wantErr: "funding sources carry",
		},
		{
			name: "funded output misses change script",
			corrupt: func(_ *BatchAnchorRequest,
				funded *psbt.Packet, _ *fakeBatchDriver) {

				funded.UnsignedTx.TxOut[1].PkScript = []byte{
					0x53,
				}
			},
			wantErr: "does not carry the derived change script",
		},
		{
			name: "committed change amount diverges",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					r.outputs[1].amount--
				}
			},
			wantErr: "committed change amount",
		},
		{
			name: "committed change root diverges",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					r.outputs[1].taprootMerkleRoot[0] ^= 1
				}
			},
			wantErr: "does not reproduce the derived script",
		},
		{
			name: "committed change asset diverges",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					r.outputs[1].assetRef =
						tapsdk.AssetRefFromAssetID(
							tapsdk.AssetID{0xff},
						)
				}
			},
			wantErr: "change output 0 asset ref mismatch",
		},
		{
			name: "committed change script mode diverges",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					r.outputs[1].scriptMode =
						tapsdk.CustomAssetScriptOPTrue
				}
			},
			wantErr: "is not external",
		},
		{
			name: "committed change outpoint diverges",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					r.outputs[1].anchorOutpoint.Index++
				}
			},
			wantErr: "change outpoint mismatch",
		},
		{
			name: "committed change output missing",
			corrupt: func(_ *BatchAnchorRequest, _ *psbt.Packet,
				fake *fakeBatchDriver) {

				fake.mutateCommit = func(r *commitResult) {
					r.outputs = r.outputs[:1]
				}
			},
			wantErr: "misses output",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			committer, req, template := newBatchAnchorFixture(t)
			template = addBatchAnchorChange(t, req, template)
			fake, ok := committer.driver.(*fakeBatchDriver)
			require.True(t, ok)
			ctx := context.Background()

			derived, err := committer.DeriveScript(
				ctx, req, template,
			)
			require.NoError(t, err)

			funded := fundedFromTemplate(t, template, derived)
			funded.UnsignedTx.TxOut[1].PkScript = append(
				[]byte(nil), derived.ChangePkScript...,
			)
			test.corrupt(req, funded, fake)

			_, err = committer.Commit(ctx, req, funded, derived)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

// TestBatchAnchorRequestGuards checks invalid requests.
func TestBatchAnchorRequestGuards(t *testing.T) {
	t.Parallel()

	committer, req, template := newBatchAnchorFixture(t)
	ctx := context.Background()

	foreignTx := template.UnsignedTx.Copy()
	foreignTx.TxIn[0].PreviousOutPoint.Index++
	foreign, err := psbt.NewFromUnsignedTx(foreignTx)
	require.NoError(t, err)
	_, err = committer.DeriveScript(ctx, req, foreign)
	require.ErrorContains(t, err, "does not spend the funding anchor")

	broken := *req
	brokenSource := req.Sources[0]
	brokenSource.ProofFile = nil
	broken.Sources = []BatchAnchorSource{brokenSource}
	_, err = committer.DeriveScript(ctx, &broken, template)
	require.ErrorContains(t, err, "proof file 0 is required")

	broken = *req
	broken.Amount = 0
	_, err = committer.DeriveScript(ctx, &broken, template)
	require.ErrorContains(t, err, "asset amount is required")

	broken = *req
	brokenSource = req.Sources[0]
	brokenSource.Verifier = nil
	broken.Sources = []BatchAnchorSource{brokenSource}
	_, err = committer.DeriveScript(ctx, &broken, template)
	require.ErrorContains(t, err, "proof verifier 0 is required")

	broken = *req
	broken.Sources = append(
		[]BatchAnchorSource(nil), req.Sources[0], req.Sources[0],
	)
	broken.Sources[0].Amount = 750
	broken.Sources[1].Amount = 750
	_, err = committer.DeriveScript(ctx, &broken, template)
	require.ErrorContains(t, err, "proof file 1 is repeated")

	broken = *req
	broken.Sources = append([]BatchAnchorSource(nil), req.Sources...)
	second := req.Sources[0]
	second.ProofFile = []byte("second-proof")
	second.AnchorOutpoint.Index++
	second.Amount = 1
	broken.Sources[0].Amount = math.MaxUint64
	broken.Sources = append(broken.Sources, second)
	brokenTx := template.UnsignedTx.Copy()
	brokenTx.AddTxIn(wire.NewTxIn(&second.AnchorOutpoint, nil, nil))
	brokenPacket, err := psbt.NewFromUnsignedTx(brokenTx)
	require.NoError(t, err)
	_, err = committer.DeriveScript(ctx, &broken, brokenPacket)
	require.ErrorContains(t, err, "amount overflow")
}

// TestBalanceTemplatePreservesOutputMetadata checks BIP-371 output fields.
func TestBalanceTemplatePreservesOutputMetadata(t *testing.T) {
	t.Parallel()

	_, _, template := newBatchAnchorFixture(t)
	template.Outputs[0].TaprootInternalKey = make([]byte, 32)
	template.Outputs[0].TaprootTapTree = []byte{0x03, 0x04}

	balanced, err := balanceTemplate(template)
	require.NoError(t, err)
	require.Equal(
		t, template.Outputs[0].TaprootInternalKey,
		balanced.Outputs[0].TaprootInternalKey,
	)
	require.Equal(
		t, template.Outputs[0].TaprootTapTree,
		balanced.Outputs[0].TaprootTapTree,
	)
}

// TestBatchAnchorMultiSourceRequest checks a batch with two issuances.
func TestBatchAnchorMultiSourceRequest(t *testing.T) {
	t.Parallel()

	committer, req, template := newBatchAnchorFixture(t)
	groupKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	parsedGroupKey, err := tapsdk.ParsePubKey(
		groupKey.PubKey().SerializeCompressed(),
	)
	require.NoError(t, err)
	req.AssetRef = tapsdk.AssetRefFromGroupKey(parsedGroupKey)
	req.Sources[0].Amount = 750

	anchorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	secondOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("second-tranche")),
		Index: 2,
	}
	req.Sources = append(req.Sources, BatchAnchorSource{
		ProofFile:         []byte("second-tranche-proof"),
		Amount:            750,
		Verifier:          staticProofVerifier{},
		AnchorOutpoint:    secondOutpoint,
		AnchorInternalKey: anchorKey.PubKey(),
	})
	tx := template.UnsignedTx.Copy()
	tx.AddTxIn(wire.NewTxIn(&secondOutpoint, nil, nil))
	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	request, _, err := committer.buildRequest(req, packet)
	require.NoError(t, err)
	require.Len(t, request.Inputs, 2)
	require.True(t, request.Inputs[0].AssetRef.Equivalent(req.AssetRef))
	require.True(t, request.Inputs[1].AssetRef.Equivalent(req.AssetRef))

	derived, err := committer.DeriveScript(t.Context(), req, packet)
	require.NoError(t, err)
	funded := fundedFromTemplate(t, packet, derived)
	commit, err := committer.Commit(t.Context(), req, funded, derived)
	require.NoError(t, err)
	require.Len(t, commit.RootSource.Inputs, 2)
	require.Equal(t, uint64(750), commit.RootSource.Inputs[0].Amount)
	require.Equal(t, uint64(750), commit.RootSource.Inputs[1].Amount)

	fake, ok := committer.driver.(*fakeBatchDriver)
	require.True(t, ok)
	require.Len(t, fake.paths, 2)
	for _, path := range fake.paths {
		require.Empty(t, path.Steps[0].CoInputPaths)
	}
}

// TestBatchSourcesSharedAnchor allows distinct assets in one Bitcoin output.
func TestBatchSourcesSharedAnchor(t *testing.T) {
	t.Parallel()

	_, req, template := newBatchAnchorFixture(t)
	req.Sources[0].Amount = 750
	second := req.Sources[0]
	second.ProofFile = []byte("second-proof")
	second.Amount = 750
	req.Sources = append(req.Sources, second)

	total, err := validateBatchSources(req.Sources, template.UnsignedTx)
	require.NoError(t, err)
	require.Equal(t, req.Amount, total)
}

// TestBatchAnchorPublish checks publish errors and unknown outcomes.
func TestBatchAnchorPublish(t *testing.T) {
	t.Parallel()

	committer, _, _ := newBatchAnchorFixture(t)
	fake, ok := committer.driver.(*fakeBatchDriver)
	require.True(t, ok)

	err := committer.Publish(t.Context(), nil, []byte("psbt"))
	require.ErrorContains(t, err, "required")

	err = committer.Publish(
		t.Context(), []byte("package"), []byte("psbt"),
	)
	require.NoError(t, err)
	require.Equal(t, 1, fake.published)

	fake.publishErr = &tapsdk.CustomAnchorPublishAttemptError{
		Err:            fmt.Errorf("transport failed"),
		OutcomeUnknown: true,
	}
	err = committer.Publish(
		t.Context(), []byte("package"), []byte("psbt"),
	)
	require.ErrorIs(t, err, ErrReconciliationRequired)
}

// TestBatchAnchorJournal checks completed replay and preparation retries.
func TestBatchAnchorJournal(t *testing.T) {
	t.Parallel()

	t.Run("completed replay", func(t *testing.T) {
		committer, req, template := newBatchAnchorFixture(t)
		fake, ok := committer.driver.(*fakeBatchDriver)
		require.True(t, ok)
		derived, err := committer.DeriveScript(
			t.Context(), req, template,
		)
		require.NoError(t, err)
		funded := fundedFromTemplate(t, template, derived)

		_, err = committer.Commit(t.Context(), req, funded, derived)
		require.NoError(t, err)
		commits := fake.commits
		_, err = committer.Commit(t.Context(), req, funded, derived)
		require.NoError(t, err)
		require.Equal(t, commits, fake.commits)

		changed := *req
		changed.Sources = append(
			[]BatchAnchorSource(nil), req.Sources...,
		)
		changed.Sources[0].Witness = [][]byte{{0x51}}
		_, err = committer.Commit(
			t.Context(), &changed, funded, derived,
		)
		require.ErrorContains(t, err, "different request")
		require.Equal(t, commits, fake.commits)
	})

	t.Run("failed preparation retry", func(t *testing.T) {
		committer, req, template := newBatchAnchorFixture(t)
		fake, ok := committer.driver.(*fakeBatchDriver)
		require.True(t, ok)
		derived, err := committer.DeriveScript(
			t.Context(), req, template,
		)
		require.NoError(t, err)
		funded := fundedFromTemplate(t, template, derived)
		fake.commitErr = &tapsdk.CustomAnchorCommitAttemptError{
			Err:            fmt.Errorf("transport failed"),
			OutcomeUnknown: true,
		}

		_, err = committer.Commit(t.Context(), req, funded, derived)
		require.ErrorContains(t, err, "transport failed")
		commits := fake.commits
		fake.commitErr = nil
		_, err = committer.Commit(t.Context(), req, funded, derived)
		require.NoError(t, err)
		require.Equal(t, commits+1, fake.commits)
	})

	t.Run("request cancellation after commit", func(t *testing.T) {
		committer, req, template := newBatchAnchorFixture(t)
		fake, ok := committer.driver.(*fakeBatchDriver)
		require.True(t, ok)
		derived, err := committer.DeriveScript(
			t.Context(), req, template,
		)
		require.NoError(t, err)
		funded := fundedFromTemplate(t, template, derived)
		ctx, cancel := context.WithCancel(t.Context())
		fake.afterCommit = cancel

		_, err = committer.Commit(ctx, req, funded, derived)
		require.NoError(t, err)
		commits := fake.commits
		fake.afterCommit = nil
		_, err = committer.Commit(t.Context(), req, funded, derived)
		require.NoError(t, err)
		require.Equal(t, commits, fake.commits)
	})
}
