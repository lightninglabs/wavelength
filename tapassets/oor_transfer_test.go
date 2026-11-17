package tapassets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/tap-sdk/customanchor"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/stretchr/testify/require"
)

// fakeOORDriver commits transitions whose asset roots depend on each output's
// script key and anchor index, as split commitments do.
type fakeOORDriver struct {
	requests []*customanchor.Request
	packages map[string]*commitResult
	previews int

	// previewRoot overrides the previewed asset root of an output.
	previewRoot func(output customanchor.AssetOutput) tapsdk.Hash

	// mutate tampers with a commit before it is returned.
	mutate func(request *customanchor.Request, result *commitResult)
}

// fakeAssetRoot derives an asset root from what a real one commits to.
func fakeAssetRoot(output customanchor.AssetOutput) tapsdk.Hash {
	key := output.Script.OPTrue.InternalKey.RawKeyBytes

	return sha256.Sum256(
		fmt.Appendf(
			nil, "asset/%x/%d/%d", key[:], output.AnchorOutputIndex,
			output.Amount,
		),
	)
}

// fakeRoots returns an output's asset root and the merkle root of its
// anchor's leaves composed with it.
func fakeRoots(output customanchor.AssetOutput, assetRoot tapsdk.Hash) (
	*btcec.PublicKey, tapsdk.Hash, error) {

	internalKey, err := btcec.ParsePubKey(
		output.Anchor.InternalKey.PubKey[:],
	)
	if err != nil {
		return nil, tapsdk.Hash{}, err
	}
	leaves := make(
		[]txscript.TapLeaf, len(output.Anchor.Tapscript.TapLeaves),
	)
	for idx, leaf := range output.Anchor.Tapscript.TapLeaves {
		leaves[idx] = txscript.NewBaseTapLeaf(leaf.Script)
	}
	policyRoot := txscript.AssembleTaprootScriptTree(leaves...).
		RootNode.TapHash()
	left, right := policyRoot, chainhash.Hash(assetRoot)
	if bytes.Compare(left[:], right[:]) > 0 {
		left, right = right, left
	}
	merkleRoot := chainhash.TaggedHash(
		chainhash.TagTapBranch, left[:], right[:],
	)

	return internalKey, tapsdk.Hash(*merkleRoot), nil
}

func (f *fakeOORDriver) Preview(_ context.Context,
	request *customanchor.Request, _ customanchor.ConfirmedProofVerifier) (
	[]outputCommitmentPreview, error) {

	f.previews++
	previews := make([]outputCommitmentPreview, len(request.Outputs))
	for idx, output := range request.Outputs {
		assetRoot := fakeAssetRoot(output)
		if f.previewRoot != nil {
			assetRoot = f.previewRoot(output)
		}
		_, merkleRoot, err := fakeRoots(output, assetRoot)
		if err != nil {
			return nil, err
		}
		previews[idx] = outputCommitmentPreview{
			logicalOutputID:   output.ID,
			anchorOutputIndex: output.AnchorOutputIndex,
			assetRoot:         assetRoot,
			merkleRoot:        merkleRoot,
		}
	}

	return previews, nil
}

func (f *fakeOORDriver) Commit(_ context.Context, request *customanchor.Request,
	_ customanchor.ConfirmedProofVerifier) (*commitResult, error) {

	f.requests = append(f.requests, request.Clone())

	packet, err := psbtutil.Parse(request.AnchorPSBT)
	if err != nil {
		return nil, err
	}
	committedTx := packet.UnsignedTx.Copy()

	result := &commitResult{
		fundingMode: customanchor.FundingCallerFundedExact,
	}
	remaining := make([]uint64, len(request.Inputs))
	issuances := make([]tapsdk.AssetID, len(request.Inputs))
	for idx, input := range request.Inputs {
		issuanceID, err := fakeInputIssuance(input)
		if err != nil {
			return nil, err
		}
		remaining[idx] = input.Amount
		issuances[idx] = issuanceID
		// Each issuance moves in its own virtual packet.
		result.inputs = append(result.inputs, commitInput{
			logicalInputID: input.ID,
			packetIndex:    uint32(idx),
			anchorOutpoint: fakeAnchor(input),
			assetRef:       input.AssetRef,
			issuanceID:     issuanceID,
			amount:         input.Amount,
			proofSource: commitProofSource{
				kind: customanchor.ProofSourceConfirmedFile,
				blob: bytes.Clone(
					input.ProofPath.ConfirmedBaseProof,
				),
			},
		})
	}

	const opTrueMode = customanchor.AssetScriptOPTrue
	for _, output := range request.Outputs {
		assetRoot := fakeAssetRoot(output)
		internalKey, merkleRoot, err := fakeRoots(output, assetRoot)
		if err != nil {
			return nil, err
		}
		script, err := composedScript(internalKey, merkleRoot)
		if err != nil {
			return nil, err
		}
		committedTx.TxOut[output.AnchorOutputIndex].PkScript = script

		amount := output.Amount
		for idx := range remaining {
			piece := min(amount, remaining[idx])
			if piece == 0 {
				continue
			}
			remaining[idx] -= piece
			amount -= piece
			result.outputs = append(result.outputs, commitOutput{
				logicalOutputID:   output.ID,
				packetIndex:       uint32(idx),
				anchorOutputIndex: output.AnchorOutputIndex,
				anchorValueSat:    int64(output.AnchorValueSat),
				assetRef:          output.AssetRef,
				issuanceID:        issuances[idx],
				amount:            piece,
				taprootAssetRoot:  assetRoot,
				taprootMerkleRoot: merkleRoot,
				scriptMode:        opTrueMode,
				opTrueWitness:     [][]byte{{0x51}},
			})
		}
		if amount != 0 {
			return nil, fmt.Errorf("fake output exceeds inputs")
		}
	}

	txid := committedTx.TxHash()
	for idx := range result.outputs {
		output := &result.outputs[idx]
		anchor := wire.OutPoint{
			Hash: txid, Index: output.anchorOutputIndex,
		}
		output.anchorOutpoint = sdkOutpoint(anchor)
		output.proofBlob = fakeProof(anchor)
	}
	packet.UnsignedTx = committedTx
	result.anchorPSBT, err = psbtutil.Serialize(packet)
	if err != nil {
		return nil, err
	}
	result.packageBytes = fmt.Appendf(nil, "package/%s", txid)

	if f.mutate != nil {
		f.mutate(request, result)
	}
	if f.packages == nil {
		f.packages = make(map[string]*commitResult)
	}
	f.packages[string(result.packageBytes)] = result

	return result, nil
}

// fakeProof is a fake transition proof that names its anchor.
func fakeProof(anchor wire.OutPoint) []byte {
	return []byte("anchor/" + anchor.String())
}

// fakeAnchor returns the anchor the last step of a fake input's proof path
// names.
func fakeAnchor(input customanchor.AssetInput) tapsdk.Outpoint {
	if input.ProofPath == nil || len(input.ProofPath.Steps) == 0 {
		return tapsdk.Outpoint{}
	}
	step := input.ProofPath.Steps[len(input.ProofPath.Steps)-1]
	anchor, err := wire.NewOutPointFromString(
		strings.TrimPrefix(
			string(step.TransitionProof),
			"anchor/",
		),
	)
	if err != nil {
		return tapsdk.Outpoint{}
	}

	return sdkOutpoint(*anchor)
}

func (f *fakeOORDriver) DecodePackage(encoded []byte) (*commitResult, error) {
	result := f.packages[string(encoded)]
	if result == nil {
		return nil, fmt.Errorf("fake package not found")
	}

	return result, nil
}

type oorFixture struct {
	operator  *btcec.PublicKey
	sender    *btcec.PublicKey
	change    *btcec.PublicKey
	recipient *btcec.PublicKey
	vtxo      oortx.SpentVTXORef
	carrier   oortx.CheckpointOutput
}

func newOORFixture(t *testing.T) *oorFixture {
	t.Helper()

	key := func(seed byte) *btcec.PublicKey {
		priv, _ := btcec.PrivKeyFromBytes(
			bytes.Repeat(
				[]byte{seed}, 32,
			),
		)

		return priv.PubKey()
	}
	f := &oorFixture{
		operator:  key(1),
		sender:    key(2),
		change:    key(4),
		recipient: key(3),
	}

	_, vtxoScript, err := arkscript.EncodeStandardVTXOArtifacts(
		f.sender, f.operator, 144,
	)
	require.NoError(t, err)
	f.vtxo = oortx.SpentVTXORef{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0xaa,
			},
			Index: 1,
		},
		Output: wire.NewTxOut(1_000, vtxoScript),
	}

	_, carrierScript, err := arkscript.EncodeStandardVTXOArtifacts(
		f.operator, f.operator, 144,
	)
	require.NoError(t, err)
	f.carrier = oortx.CheckpointOutput{
		Txid: chainhash.Hash{
			0xbb,
		},
		Output: wire.NewTxOut(10_000, carrierScript),
	}

	return f
}

func (f *oorFixture) policy(t *testing.T, owner *btcec.PublicKey) []byte {
	t.Helper()

	template, err := arkscript.EncodeStandardVTXOTemplate(
		owner, f.operator, 144,
	)
	require.NoError(t, err)

	return template
}

func (f *oorFixture) output(t *testing.T, owner *btcec.PublicKey,
	value btcutil.Amount) oortx.RecipientOutput {

	t.Helper()

	template, script, err := arkscript.EncodeStandardVTXOArtifacts(
		owner, f.operator, 144,
	)
	require.NoError(t, err)

	return oortx.RecipientOutput{
		PkScript:           script,
		Value:              value,
		VTXOPolicyTemplate: template,
	}
}

// piece returns an asset piece of the fixture VTXO in issuance id.
func (f *oorFixture) piece(id byte, amount uint64) AssetVTXOClaimInput {
	return AssetVTXOClaimInput{
		Amount: amount,
		ProofPath: &customanchor.ProofPath{
			ConfirmedBaseProof: []byte{
				id,
				0x01,
			},
			Steps: []customanchor.ProofPathStep{{
				TransitionProof: fakeProof(f.vtxo.Outpoint),
			}},
		},
		OPTrueWitness: [][]byte{
			{
				0x51,
			},
		},
	}
}

// request returns a partial send of 60 of 100 units: the recipient and the
// asset change each take a carrier from the leased float, the sender's own
// carrier returns as Bitcoin change, and the operator keeps the rest.
func (f *oorFixture) request(t *testing.T) OORTransferRequest {
	t.Helper()

	return OORTransferRequest{
		AssetRef: tapsdk.AssetRef("asset"),
		Digest: tapsdk.Hash{
			0x01,
		},
		CheckpointPolicy: arkscript.CheckpointPolicy{
			OperatorKey: f.operator,
			CSVDelay:    144,
		},
		Inputs: []OORAssetInput{{
			Checkpoint: oortx.CheckpointInput{
				SpentVTXO: f.vtxo,
				OwnerLeafScript: []byte{
					txscript.OP_1,
				},
			},
			Pieces: []AssetVTXOClaimInput{
				f.piece(0x07, 100),
			},
		}},
		Checkpoints: []oortx.CheckpointOutput{
			f.carrier,
		},
		AssetOutputs: []OORAssetOutput{{
			PolicyTemplate: f.policy(t, f.recipient),
			Amount:         60,
			Value:          1_000,
		}, {
			PolicyTemplate: f.policy(t, f.sender),
			Amount:         40,
			Value:          1_000,
		}},
		Outputs: []oortx.RecipientOutput{
			f.output(t, f.change, 1_000),
			f.output(t, f.operator, 8_000),
		},
	}
}

func TestBuildOORTransfer(t *testing.T) {
	f := newOORFixture(t)
	driver := &fakeOORDriver{}
	req := f.request(t)

	transfer, err := buildOORTransfer(t.Context(), driver, req)
	require.NoError(t, err)

	// The checkpoint keeps the VTXO's carrier and commits to the asset
	// beside its policy.
	require.Len(t, transfer.Checkpoints, 1)
	checkpoint := transfer.Checkpoints[0].PSBT.UnsignedTx
	require.Equal(t, f.vtxo.Outpoint, checkpoint.TxIn[0].PreviousOutPoint)
	require.EqualValues(t, 1_000, checkpoint.TxOut[0].Value)
	checkpointScript, err := arkscript.CheckpointPkScript(
		req.CheckpointPolicy, []byte{txscript.OP_1},
	)
	require.NoError(t, err)
	require.NotEqual(t, checkpointScript, checkpoint.TxOut[0].PkScript)
	tapscript, err := arkscript.CheckpointTapScript(
		req.CheckpointPolicy, []byte{txscript.OP_1},
	)
	require.NoError(t, err)
	composed, err := composedPolicyScript(
		&arkscript.ARKNUMSKey, tapscript.Leaves,
		transfer.CheckpointAssetRoots[0],
	)
	require.NoError(t, err)
	require.Equal(t, composed, checkpoint.TxOut[0].PkScript)

	// The Ark transaction spends the checkpoint and the leased carrier,
	// and lays its outputs out canonically.
	ark := transfer.Ark.UnsignedTx
	require.Len(t, ark.TxIn, 2)
	require.NoError(t, arktx.ValidateCanonicalTx(ark))
	require.Len(t, transfer.Recipients, 4)
	rebuilt, err := oortx.BuildArkPSBT(
		[]oortx.CheckpointOutput{
			{
				Txid:   checkpoint.TxHash(),
				Output: checkpoint.TxOut[0],
			},
			f.carrier,
		},
		transfer.Recipients,
	)
	require.NoError(t, err)
	require.Equal(t, ark.TxHash(), rebuilt.UnsignedTx.TxHash())

	units := make(map[string]uint64)
	var bitcoin btcutil.Amount
	for idx, recipient := range transfer.Recipients {
		require.Equal(t, ark.TxOut[idx].PkScript, recipient.PkScript)
		require.NoError(t, recipient.ValidateAsset())
		if recipient.Asset == nil {
			bitcoin += recipient.Value
			continue
		}
		require.EqualValues(t, 1_000, recipient.Value)
		require.Equal(t, "asset", recipient.Asset.Ref)
		routing, err := recipient.RoutingPkScript()
		require.NoError(t, err)
		units[string(routing)] = recipient.Asset.Amount
	}
	require.EqualValues(t, 9_000, bitcoin)
	require.Equal(t, map[string]uint64{
		string(f.output(t, f.recipient, 0).PkScript): 60,
		string(f.output(t, f.sender, 0).PkScript):    40,
	}, units)

	// The Ark transition spends the asset through the checkpoint's step,
	// on top of the VTXO's own path.
	require.Len(t, driver.requests, 2)
	arkInputs := driver.requests[1].Inputs
	require.Len(t, arkInputs, 1)
	require.Len(t, arkInputs[0].ProofPath.Steps, 2)
	require.Equal(
		t,
		fakeProof(
			wire.OutPoint{
				Hash: checkpoint.TxHash(),
			},
		),
		arkInputs[0].ProofPath.Steps[1].TransitionProof,
	)

	// Every new asset VTXO reads its piece from the Ark package.
	committed, err := driver.DecodePackage(transfer.ArkPackage)
	require.NoError(t, err)
	for idx, recipient := range transfer.Recipients {
		if recipient.Asset == nil {
			continue
		}
		pieces, err := claimInputsFromCommit(
			committed, wire.OutPoint{
				Hash:  ark.TxHash(),
				Index: uint32(idx),
			},
		)
		require.NoError(t, err)
		require.Len(t, pieces, 1)
		require.Equal(t, recipient.Asset.Amount, pieces[0].Amount)
		require.Equal(
			t,
			fakeProof(
				wire.OutPoint{
					Hash:  ark.TxHash(),
					Index: uint32(idx),
				},
			),
			pieces[0].ProofPath.Steps[0].TransitionProof,
		)
	}
}

func TestBuildOORTransferFullSend(t *testing.T) {
	f := newOORFixture(t)
	req := f.request(t)
	req.AssetOutputs = req.AssetOutputs[:1]
	req.AssetOutputs[0].Amount = 100
	req.Outputs[1] = f.output(t, f.operator, 9_000)

	transfer, err := buildOORTransfer(t.Context(), &fakeOORDriver{}, req)
	require.NoError(t, err)

	var assets int
	for _, recipient := range transfer.Recipients {
		if recipient.Asset != nil {
			assets++
			require.EqualValues(t, 100, recipient.Asset.Amount)
		}
	}
	require.Equal(t, 1, assets)
}

func TestBuildOORTransferIssuances(t *testing.T) {
	f := newOORFixture(t)
	req := f.request(t)
	req.Inputs[0].Pieces = []AssetVTXOClaimInput{
		f.piece(0x07, 50), f.piece(0x08, 50),
	}
	driver := &fakeOORDriver{}

	transfer, err := buildOORTransfer(t.Context(), driver, req)
	require.NoError(t, err)

	// The recipient's 60 units span both issuances, and the change holds
	// the rest of the second.
	committed, err := driver.DecodePackage(transfer.ArkPackage)
	require.NoError(t, err)
	txid := transfer.Ark.UnsignedTx.TxHash()
	for idx, recipient := range transfer.Recipients {
		if recipient.Asset == nil {
			continue
		}
		pieces, err := claimInputsFromCommit(
			committed, wire.OutPoint{
				Hash:  txid,
				Index: uint32(idx),
			},
		)
		require.NoError(t, err)

		var total uint64
		for _, piece := range pieces {
			total += piece.Amount
		}
		require.Equal(t, recipient.Asset.Amount, total)
		if recipient.Asset.Amount == 60 {
			require.Len(t, pieces, 2)
		}
	}
}

func TestBuildOORTransferUnresolvedOrder(t *testing.T) {
	f := newOORFixture(t)

	// Every preview gives fresh roots, so no layout is ever confirmed.
	driver := &fakeOORDriver{}
	driver.previewRoot = func(customanchor.AssetOutput) tapsdk.Hash {
		return sha256.Sum256(fmt.Appendf(nil, "%d", driver.previews))
	}

	_, err := buildOORTransfer(t.Context(), driver, f.request(t))
	require.ErrorIs(t, err, ErrOOROrderingUnresolved)
	require.Len(t, driver.requests, 1)
}

func TestBuildOORTransferRejectsRequest(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(f *oorFixture, req *OORTransferRequest)
		err    string
	}{{
		name: "units not conserved",
		mutate: func(_ *oorFixture, req *OORTransferRequest) {
			req.AssetOutputs[0].Amount = 61
		},
		err: "asset outputs hold 101 units",
	}, {
		name: "two asset inputs",
		mutate: func(_ *oorFixture, req *OORTransferRequest) {
			req.Inputs = append(req.Inputs, req.Inputs[0])
		},
		err: "one asset VTXO",
	}, {
		name: "three asset outputs",
		mutate: func(_ *oorFixture, req *OORTransferRequest) {
			req.AssetOutputs = append(
				req.AssetOutputs, req.AssetOutputs[0],
			)
		},
		err: "at most 2 asset outputs",
	}, {
		name: "asset on a Bitcoin output",
		mutate: func(_ *oorFixture, req *OORTransferRequest) {
			req.Outputs[0].Asset = &oortx.RecipientAsset{}
		},
		err: "output 0 carries an asset",
	}, {
		name: "ambiguous outputs",
		mutate: func(f *oorFixture, req *OORTransferRequest) {
			req.Outputs[0] = f.output(t, f.sender, 1_000)
		},
		err: "two outputs pay 1000 to one script",
	}, {
		name: "piece without witness",
		mutate: func(_ *oorFixture, req *OORTransferRequest) {
			req.Inputs[0].Pieces[0].OPTrueWitness = nil
		},
		err: "piece 0 is incomplete",
	}, {
		name: "missing digest",
		mutate: func(_ *oorFixture, req *OORTransferRequest) {
			req.Digest = tapsdk.Hash{}
		},
		err: "transfer digest is required",
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newOORFixture(t)
			req := f.request(t)
			test.mutate(f, &req)
			driver := &fakeOORDriver{}

			_, err := buildOORTransfer(t.Context(), driver, req)
			require.ErrorContains(t, err, test.err)
			require.Empty(t, driver.requests)
		})
	}
}

func TestBuildOORTransferRejectsCommit(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(request *customanchor.Request, result *commitResult)
		err    string
	}{{
		name: "fee",
		mutate: func(_ *customanchor.Request, result *commitResult) {
			result.actualFeeSat = 1
		},
		err: "pay no fee",
	}, {
		name: "amount",
		mutate: func(_ *customanchor.Request, result *commitResult) {
			result.outputs[0].amount--
		},
		err: "issuance amount",
	}, {
		name: "asset root",
		mutate: func(_ *customanchor.Request, result *commitResult) {
			for idx := range result.outputs {
				result.outputs[idx].taprootAssetRoot[0] ^= 1
			}
		},
		err: "commit to its policy and asset root",
	}, {
		name: "script mode",
		mutate: func(_ *customanchor.Request, result *commitResult) {
			result.outputs[0].scriptMode =
				customanchor.AssetScriptExternal
		},
		err: "not OP_TRUE",
	}, {
		name: "input ref",
		mutate: func(_ *customanchor.Request, result *commitResult) {
			result.inputs[0].assetRef = "other"
		},
		err: "ref mismatch",
	}, {
		name: "extra output",
		mutate: func(_ *customanchor.Request, result *commitResult) {
			extra := result.outputs[0]
			extra.logicalOutputID = "extra"
			extra.amount = 0
			result.outputs = append(result.outputs, extra)
		},
		err: "outputs, want",
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newOORFixture(t)
			driver := &fakeOORDriver{mutate: test.mutate}

			_, err := buildOORTransfer(
				t.Context(), driver, f.request(t),
			)
			require.ErrorContains(t, err, test.err)
		})
	}
}
