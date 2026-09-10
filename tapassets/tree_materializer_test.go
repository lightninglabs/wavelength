package tapassets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"sync"
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

type fakeTreeDriver struct {
	commits     int
	requests    []*tapsdk.CustomAnchorRequest
	packages    map[string]*commitResult
	commitErr   error
	afterCommit func()

	mutate func(request *tapsdk.CustomAnchorRequest, result *commitResult)
}

func merkleRootFor(input wire.OutPoint, index uint32) tapsdk.Hash {
	seed := sha256.Sum256([]byte(fmt.Sprintf("root/%s/%d", input, index)))

	return tapsdk.Hash(seed)
}

func (f *fakeTreeDriver) Commit(_ context.Context,
	request *tapsdk.CustomAnchorRequest, _ tapsdk.ConfirmedProofVerifier) (
	*commitResult, error) {

	f.commits++
	f.requests = append(f.requests, request.Clone())
	if f.commitErr != nil {
		return nil, f.commitErr
	}
	if f.afterCommit != nil {
		f.afterCommit()
	}

	template, err := psbtutil.Parse(request.AnchorPSBT)
	if err != nil {
		return nil, err
	}
	committedTx := template.UnsignedTx.Copy()
	input := committedTx.TxIn[0].PreviousOutPoint

	result := &commitResult{
		fundingMode: tapsdk.CustomAnchorFundingCallerFundedExact,
		inputs:      make([]commitInput, len(request.Inputs)),
	}
	issuanceAmounts := make([]uint64, len(request.Inputs))
	issuanceIDs := make([]tapsdk.AssetID, len(request.Inputs))
	for idx := range request.Inputs {
		assetInput := request.Inputs[idx]
		issuanceID, err := fakeInputIssuance(assetInput)
		if err != nil {
			return nil, err
		}
		issuanceIDs[idx] = issuanceID
		issuanceAmounts[idx] = assetInput.Amount
		result.inputs[idx] = commitInput{
			logicalInputID: assetInput.ID,
			anchorOutpoint: sdkOutpoint(input),
			assetRef:       assetInput.AssetRef,
			issuanceID:     issuanceID,
			amount:         assetInput.Amount,
		}
	}

	for _, out := range request.Outputs {
		merkleRoot := merkleRootFor(input, out.AnchorOutputIndex)
		assetRoot := sha256.Sum256(
			[]byte(
				fmt.Sprintf("asset/%s/%d", input,
					out.AnchorOutputIndex),
			),
		)

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

		remaining := out.Amount
		for idx := range issuanceAmounts {
			amount := min(remaining, issuanceAmounts[idx])
			if amount == 0 {
				continue
			}
			issuanceAmounts[idx] -= amount
			remaining -= amount
			result.outputs = append(result.outputs, commitOutput{
				anchorOutputIndex: out.AnchorOutputIndex,
				anchorValueSat:    int64(out.AnchorValueSat),
				assetRef:          out.AssetRef,
				issuanceID:        issuanceIDs[idx],
				amount:            amount,
				taprootAssetRoot:  tapsdk.Hash(assetRoot),
				taprootMerkleRoot: merkleRoot,
				scriptMode: tapsdk.
					CustomAssetScriptOPTrue,
				opTrueWitness: [][]byte{{0x51}},
				proofBlob: []byte(
					fmt.Sprintf("proof/%s/%d/%x", input,
						out.AnchorOutputIndex,
						issuanceIDs[idx][0]),
				),
			})
		}
		if remaining != 0 {
			return nil, fmt.Errorf("fake output exceeds input " +
				"amount")
		}
	}

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
	if f.packages == nil {
		f.packages = make(map[string]*commitResult)
	}
	f.packages[string(result.packageBytes)] = result

	return result, nil
}

func fakeInputIssuance(input tapsdk.CustomAssetInput) (tapsdk.AssetID, error) {
	var proofFile []byte
	if len(input.ProofFile) != 0 {
		proofFile = input.ProofFile
	} else if input.ProofPath != nil {
		proofFile = input.ProofPath.ConfirmedBaseProof
	}
	if len(proofFile) == 0 {
		return tapsdk.AssetID{}, fmt.Errorf("fake input has no proof")
	}

	return tapsdk.AssetID{proofFile[0]}, nil
}

// DecodePackage restores a prior fake commit for journal replay.
func (f *fakeTreeDriver) DecodePackage(encoded []byte) (*commitResult, error) {
	result := f.packages[string(encoded)]
	if result == nil {
		return nil, fmt.Errorf("fake package not found")
	}

	return result, nil
}

type memoryStore struct {
	mu     sync.Mutex
	values map[string][]byte
}

type staticProofVerifier struct{}

func (staticProofVerifier) VerifyConfirmedProof(context.Context, []byte) (
	*tapsdk.ConfirmedProofVerification, error) {

	return &tapsdk.ConfirmedProofVerification{
		AnchorAssetInventoryComplete: true,
	}, nil
}

type recordingProofVerifier struct {
	steps []uint16
}

func (v *recordingProofVerifier) VerifyConfirmedProof(context.Context, []byte) (
	*tapsdk.ConfirmedProofVerification, error) {

	return &tapsdk.ConfirmedProofVerification{
		AnchorAssetInventoryComplete: true,
	}, nil
}

func (v *recordingProofVerifier) VerifyUnconfirmedAnchor(_ context.Context,
	transition tapsdk.UnconfirmedAnchorVerification) error {

	v.steps = append(v.steps, transition.StepIndex)

	return nil
}

func newMemoryStore() *memoryStore {
	return &memoryStore{values: make(map[string][]byte)}
}

func (s *memoryStore) Load(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	value, ok := s.values[key]
	if !ok {
		return nil, ErrStoreNotFound
	}

	return append([]byte(nil), value...), nil
}

func (s *memoryStore) Store(ctx context.Context, key string,
	value []byte) error {

	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.values[key] = append([]byte(nil), value...)

	return nil
}

type treeMaterializerFixture struct {
	structure *tree.Structure
	mat       *treeMaterializer
	driver    *fakeTreeDriver
	leaves    []tree.LeafDescriptor
	operator  *btcec.PublicKey
	batch     wire.OutPoint
	batchOut  *wire.TxOut
	rootTweak []byte
}

func newTreeMaterializerFixture(t *testing.T,
	amounts ...uint64) *treeMaterializerFixture {

	t.Helper()

	_, operatorKey := createTestPubKey(t)
	leaves := make([]tree.LeafDescriptor, len(amounts))
	var batchValue int64
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
		batchValue += int64(leaves[i].Amount)
	}

	structure, err := tree.BuildStructure(leaves, tree.StructureConfig{
		OperatorKey: operatorKey,
		Radix:       2,
	})
	require.NoError(t, err)
	require.NotNil(t, structure.AssetContext)

	sweepLeaf := txscript.NewBaseTapLeaf([]byte{0x51, 0xB2})

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
	var assetAmount uint64
	for _, amount := range amounts {
		assetAmount += amount
	}
	cfg := TreeMaterializerConfig{
		Wallet:    tapsdk.NewWallet(nil, tapsdk.NetworkRegtest),
		Store:     newMemoryStore(),
		AssetRef:  assetRef,
		SweepLeaf: sweepLeaf,
		LeafAnchor: func(node *tree.Node) (TreeLeafAnchor, error) {
			_, leafInternal := createTestPubKey(t)
			script, err := txscript.PayToTaprootScript(leafInternal)
			if err != nil {
				return TreeLeafAnchor{}, err
			}

			return TreeLeafAnchor{
				UncomposedPkScript: script,
				InternalKey:        leafInternal,
				TapLeaves: []txscript.TapLeaf{
					txscript.NewBaseTapLeaf([]byte{0x51}),
				},
			}, nil
		},
		Root: TreeRootAssetSource{
			Inputs: []TreeRootAssetInput{{
				ProofFile: []byte{
					0xAA,
				},
				Amount:         assetAmount,
				AnchorOutpoint: batchOutpoint,
				Witness: [][]byte{
					{
						0x51,
					},
				},
				Verifier: staticProofVerifier{},
			}},
			SigningTweak:  rootTweak,
			BatchPkScript: batchScript,
		},
		Digest: tapsdk.Hash{
			0xD1,
		},
	}
	mat, err := newTreeMaterializer(
		cfg, structure.AssetContext, batchOutpoint, driver,
	)
	require.NoError(t, err)

	return &treeMaterializerFixture{
		structure: structure,
		mat:       mat,
		driver:    driver,
		leaves:    leaves,
		operator:  operatorKey,
		batch:     batchOutpoint,
		batchOut:  wire.NewTxOut(batchValue, batchScript),
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

// TestTreeMaterializerConfig checks required configuration.
func TestTreeMaterializerConfig(t *testing.T) {
	t.Parallel()

	_, batchKey := createTestPubKey(t)
	batchScript, err := txscript.PayToTaprootScript(batchKey)
	require.NoError(t, err)
	rootInput := wire.OutPoint{Hash: chainhash.Hash{0x01}, Index: 1}

	valid := TreeMaterializerConfig{
		Wallet:    tapsdk.NewWallet(nil, tapsdk.NetworkRegtest),
		Store:     newMemoryStore(),
		AssetRef:  tapsdk.AssetRefFromAssetID(tapsdk.AssetID{0x01}),
		SweepLeaf: txscript.NewBaseTapLeaf([]byte{txscript.OP_TRUE}),
		LeafAnchor: func(*tree.Node) (TreeLeafAnchor, error) {
			return TreeLeafAnchor{}, nil
		},
		Root: TreeRootAssetSource{
			Inputs: []TreeRootAssetInput{{
				ProofFile: []byte{
					0x01,
				},
				Amount:         10,
				AnchorOutpoint: rootInput,
				Verifier:       staticProofVerifier{},
			}},
			SigningTweak:  bytes.Repeat([]byte{0x01}, 32),
			BatchPkScript: batchScript,
		},
		Digest: tapsdk.Hash{
			0x01,
		},
	}
	assetContext := tree.NewAssetTreeContext()
	driver := &fakeTreeDriver{}

	_, err = newTreeMaterializer(
		valid, assetContext, rootInput, driver,
	)
	require.NoError(t, err)

	tests := []struct {
		name    string
		mutate  func(*TreeMaterializerConfig)
		wantErr string
	}{
		{
			name: "store",
			mutate: func(cfg *TreeMaterializerConfig) {
				cfg.Store = nil
			},
			wantErr: "store is required",
		},
		{
			name: "asset ref",
			mutate: func(cfg *TreeMaterializerConfig) {
				cfg.AssetRef = ""
			},
			wantErr: "asset ref",
		},
		{
			name: "digest",
			mutate: func(cfg *TreeMaterializerConfig) {
				cfg.Digest = tapsdk.Hash{}
			},
			wantErr: "digest is required",
		},
		{
			name: "verifier",
			mutate: func(cfg *TreeMaterializerConfig) {
				cfg.Root.Inputs[0].Verifier = nil
			},
			wantErr: "proof verifier",
		},
		{
			name: "root tweak",
			mutate: func(cfg *TreeMaterializerConfig) {
				cfg.Root.SigningTweak = []byte{
					0x01,
				}
			},
			wantErr: "must be 32 bytes",
		},
		{
			name: "batch script",
			mutate: func(cfg *TreeMaterializerConfig) {
				cfg.Root.BatchPkScript = []byte{
					txscript.OP_TRUE,
				}
			},
			wantErr: "must be P2TR",
		},
		{
			name: "ambiguous proof source",
			mutate: func(cfg *TreeMaterializerConfig) {
				cfg.Root.Inputs[0].ProofPath = []byte{
					0x01,
				}
			},
			wantErr: "exactly one proof source",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := valid
			cfg.Root.Inputs = append(
				[]TreeRootAssetInput(nil), valid.Root.Inputs...,
			)
			test.mutate(&cfg)
			_, err := newTreeMaterializer(
				cfg, assetContext, rootInput, driver,
			)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

// TestTreePathVerifier checks existing and newly appended proof steps.
func TestTreePathVerifier(t *testing.T) {
	t.Parallel()

	base := &recordingProofVerifier{}
	previous := tapsdk.Outpoint{Txid: tapsdk.Hash{0x01}, Index: 1}
	anchor := tapsdk.Outpoint{Txid: tapsdk.Hash{0x02}, Index: 2}
	verifier := &treePathVerifier{
		base:      base,
		baseDepth: 1,
		steps: []*expectedUnconfirmedAnchor{{
			previousOutpoint: previous,
			previousOutpoints: []tapsdk.Outpoint{
				previous,
			},
			anchorOutpoint: anchor,
			transaction: []byte{
				0x03,
			},
		}},
	}

	err := verifier.VerifyUnconfirmedAnchor(
		t.Context(), tapsdk.UnconfirmedAnchorVerification{
			StepIndex: 0,
		},
	)
	require.NoError(t, err)
	require.Equal(t, []uint16{0}, base.steps)

	transition := tapsdk.UnconfirmedAnchorVerification{
		StepIndex:              1,
		PreviousAnchorOutpoint: previous,
		PreviousAnchorOutpoints: []tapsdk.Outpoint{
			previous,
		},
		AnchorOutpoint: anchor,
		AnchorTransaction: []byte{
			0x03,
		},
	}
	require.NoError(
		t,
		verifier.VerifyUnconfirmedAnchor(
			t.Context(), transition,
		),
	)

	transition.PreviousAnchorOutpoints = nil
	err = verifier.VerifyUnconfirmedAnchor(t.Context(), transition)
	require.ErrorContains(t, err, "input outpoints mismatch")
}

// TestTreeMaterializerBuildsAssetTree tests a three-leaf asset tree.
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

	require.Equal(t, 5, f.driver.commits)

	built := &tree.Tree{
		Root:          f.structure.Root,
		BatchOutpoint: f.batch,
		BatchOutput:   f.batchOut,
	}
	require.NoError(t, built.Verify())

	assetCtx := f.structure.AssetContext
	require.Equal(t, f.rootTweak, assetCtx.SigningTweak(f.batch))

	for node := range f.structure.Root.NodesIter() {
		require.NotEmpty(t, node.Outputs)
		require.NotNil(t, node.FinalKey)
		require.NotEmpty(t, assetCtx.SigningTweak(node.Input))
		require.NotEmpty(t, assetCtx.SealedPackage(node.Input))

		expectedOutputs := len(node.Children) + 1
		if len(node.Children) == 0 {
			expectedOutputs = 2
		}
		require.Len(t, node.Outputs, expectedOutputs)

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

	rootScript, err := txscript.PayToTaprootScript(
		f.structure.Root.FinalKey,
	)
	require.NoError(t, err)
	require.Equal(t, f.batchOut.PkScript, rootScript)
}

// TestTreeMaterializerMultipleIssuances carries two issuances through a tree.
func TestTreeMaterializerMultipleIssuances(t *testing.T) {
	t.Parallel()

	f := newTreeMaterializerFixture(t, 800, 700)
	groupKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	sdkGroupKey, err := tapsdk.ParsePubKey(
		groupKey.PubKey().SerializeCompressed(),
	)
	require.NoError(t, err)

	cfg := f.mat.cfg
	cfg.AssetRef = tapsdk.AssetRefFromGroupKey(sdkGroupKey)
	cfg.Root.Inputs = []TreeRootAssetInput{
		{
			ProofFile: []byte{
				0xA1,
			},
			Amount:         750,
			AnchorOutpoint: f.batch,
			Witness: [][]byte{
				{
					0x51,
				},
			},
			Verifier: staticProofVerifier{},
		},
		{
			ProofFile: []byte{
				0xB2,
			},
			Amount:         750,
			AnchorOutpoint: f.batch,
			Witness: [][]byte{
				{
					0x51,
				},
			},
			Verifier: staticProofVerifier{},
		},
	}
	driver := &fakeTreeDriver{}
	materializer, err := newTreeMaterializer(
		cfg, f.structure.AssetContext, f.batch, driver,
	)
	require.NoError(t, err)

	err = tree.Materialize(
		t.Context(), f.structure.Root, tree.MaterializeParams{
			Input: f.batch,
		},
		materializer,
	)
	require.NoError(t, err)
	require.Equal(t, 3, driver.commits)
	require.Len(t, driver.requests[0].Inputs, 2)

	var hasMultiIssuanceChild bool
	for _, request := range driver.requests[1:] {
		if len(request.Inputs) == 2 {
			hasMultiIssuanceChild = true
		}
	}
	require.True(t, hasMultiIssuanceChild)

	built := &tree.Tree{
		Root:          f.structure.Root,
		BatchOutpoint: f.batch,
		BatchOutput:   f.batchOut,
	}
	require.NoError(t, built.Verify())
}

// TestTreeMaterializerCommitCache tests completed commit replay and retries.
func TestTreeMaterializerCommitCache(t *testing.T) {
	t.Parallel()

	f := newTreeMaterializerFixture(t, 800, 200)
	err := tree.Materialize(
		t.Context(), f.structure.Root, tree.MaterializeParams{
			Input: f.batch,
		}, f.mat,
	)
	require.NoError(t, err)
	require.NotEmpty(t, f.driver.requests)

	commits := f.driver.commits
	request := f.driver.requests[0]
	_, err = f.mat.commitDurably(t.Context(), f.batch, request, nil)
	require.NoError(t, err)
	require.Equal(t, commits, f.driver.commits)

	changed := request.Clone()
	changed.Outputs[0].ID += "-changed"
	_, err = f.mat.commitDurably(t.Context(), f.batch, changed, nil)
	require.ErrorContains(t, err, "different request")
	require.Equal(t, commits, f.driver.commits)

	retryInput := f.batch
	retryInput.Index++
	f.driver.commitErr = &tapsdk.CustomAnchorCommitAttemptError{
		Err:            fmt.Errorf("transport failed"),
		OutcomeUnknown: true,
	}
	_, err = f.mat.commitDurably(
		t.Context(), retryInput, request, nil,
	)
	require.ErrorContains(t, err, "transport failed")
	failedCommits := f.driver.commits

	f.driver.commitErr = nil
	_, err = f.mat.commitDurably(
		t.Context(), retryInput, request, nil,
	)
	require.NoError(t, err)
	require.Equal(t, failedCommits+1, f.driver.commits)

	retryCommits := f.driver.commits
	_, err = f.mat.commitDurably(
		t.Context(), retryInput, request, nil,
	)
	require.NoError(t, err)
	require.Equal(t, retryCommits, f.driver.commits)

	canceledInput := retryInput
	canceledInput.Index++
	ctx, cancel := context.WithCancel(t.Context())
	f.driver.afterCommit = cancel
	_, err = f.mat.commitDurably(ctx, canceledInput, request, nil)
	require.NoError(t, err)
	f.driver.afterCommit = nil

	_, err = f.mat.commitDurably(
		t.Context(), canceledInput, request, nil,
	)
	require.NoError(t, err)
}

// TestTreeMaterializerRejectsZeroAmountChild rejects empty asset outputs.
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

// TestTreeMaterializerRejectsUnknownInput rejects an unrecorded outpoint.
func TestTreeMaterializerRejectsUnknownInput(t *testing.T) {
	t.Parallel()

	f := newTreeMaterializerFixture(t, 800, 200)
	unknown := f.batch
	unknown.Index++
	_, err := f.mat.MaterializeNode(
		t.Context(), f.structure.Root, tree.MaterializeParams{
			Input: unknown,
		},
	)
	require.ErrorContains(t, err, "unknown tree input")
}

// TestTreeMaterializerRejectsTamperedCommit checks commit validation.
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
			name: "input asset drift",
			mutate: func(_ *tapsdk.CustomAnchorRequest,
				result *commitResult) {

				result.inputs[0].assetRef =
					tapsdk.AssetRefFromAssetID(
						tapsdk.AssetID{0xFF},
					)
			},
			wantErr: "ref mismatch",
		},
		{
			name: "input amount drift",
			mutate: func(_ *tapsdk.CustomAnchorRequest,
				result *commitResult) {

				result.inputs[0].amount++
			},
			wantErr: "input 0 amount",
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
			wantErr: "issuance amount",
		},
		{
			name: "output asset drift",
			mutate: func(_ *tapsdk.CustomAnchorRequest,
				result *commitResult) {

				result.outputs[0].assetRef =
					tapsdk.AssetRefFromAssetID(
						tapsdk.AssetID{0xFF},
					)
			},
			wantErr: "asset ref mismatch",
		},
		{
			name: "output outpoint drift",
			mutate: func(_ *tapsdk.CustomAnchorRequest,
				result *commitResult) {

				result.outputs[0].anchorOutpoint.Index++
			},
			wantErr: "anchor outpoint mismatch",
		},
		{
			name: "missing asset root",
			mutate: func(_ *tapsdk.CustomAnchorRequest,
				result *commitResult) {

				output := &result.outputs[0]
				output.taprootAssetRoot = tapsdk.Hash{}
			},
			wantErr: "misses a taproot root",
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
				txid := packet.UnsignedTx.TxHash()
				for idx := range result.outputs {
					output := &result.outputs[idx]
					outputIndex := output.anchorOutputIndex
					output.anchorOutpoint = sdkOutpoint(
						wire.OutPoint{
							Hash:  txid,
							Index: outputIndex,
						},
					)
				}
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

// TestBuildAssetTree checks request validation before tree construction.
func TestBuildAssetTree(t *testing.T) {
	t.Parallel()

	f := newTreeMaterializerFixture(t, 800, 200, 500)

	req := AssetTreeRequest{
		Leaves:        append([]tree.LeafDescriptor(nil), f.leaves...),
		OperatorKey:   f.operator,
		Radix:         2,
		BatchOutpoint: f.batch,
		BatchOutput:   f.batchOut,
		AssetAmount:   1_500,
	}

	short := req
	short.AssetAmount = 1_499
	_, err := buildAssetTree(
		t.Context(), f.mat.cfg, short, f.driver,
	)
	require.ErrorContains(t, err, "batch asset amount")

	wrongValue := req
	wrongValue.BatchOutput = wire.NewTxOut(29_999, f.batchOut.PkScript)
	_, err = buildAssetTree(
		t.Context(), f.mat.cfg, wrongValue, f.driver,
	)
	require.ErrorContains(t, err, "batch output value")

	wrongScript := req
	wrongScript.BatchOutput = wire.NewTxOut(30_000, []byte{0x51})
	_, err = buildAssetTree(
		t.Context(), f.mat.cfg, wrongScript, f.driver,
	)
	require.ErrorContains(t, err, "root asset source")

	duplicate := req
	duplicate.Leaves = append([]tree.LeafDescriptor(nil), req.Leaves...)
	duplicate.Leaves[1].CoSignerKey = duplicate.Leaves[0].CoSignerKey
	_, err = buildAssetTree(
		t.Context(), f.mat.cfg, duplicate, f.driver,
	)
	require.ErrorContains(t, err, "repeats a cosigner")

	overflow := req
	overflow.Leaves = append([]tree.LeafDescriptor(nil), req.Leaves...)
	overflow.Leaves[0].AssetAmount = math.MaxUint64
	_, err = buildAssetTree(
		t.Context(), f.mat.cfg, overflow, f.driver,
	)
	require.ErrorContains(t, err, "asset amount overflow")

	built, err := buildAssetTree(
		t.Context(), f.mat.cfg, req, f.driver,
	)
	require.NoError(t, err)
	require.NotNil(t, built.AssetContext)
	require.NoError(t, built.Verify())

	err = validateTreeProofCapacity(
		f.mat.cfg.Root, tapsdk.AssetProofPathMaxDepth+1,
	)
	require.ErrorContains(t, err, "cannot add")
}
