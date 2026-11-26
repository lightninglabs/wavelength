package tapassets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
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

	// reverseMerges lists a merged packet's inputs in reverse request
	// order.
	reverseMerges bool
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

	// Each issuance moves in its own virtual packet, which merges every
	// input of the issuance in request order, or in reverse with
	// reverseMerges.
	var (
		remaining []uint64
		issuances []tapsdk.AssetID
	)
	packets := make(map[tapsdk.AssetID]int)
	inputs := request.Inputs
	if f.reverseMerges {
		inputs = slices.Clone(inputs)
		slices.Reverse(inputs)
	}
	for _, input := range inputs {
		issuanceID, err := fakeInputIssuance(input)
		if err != nil {
			return nil, err
		}
		packet, ok := packets[issuanceID]
		if !ok {
			packet = len(remaining)
			packets[issuanceID] = packet
			remaining = append(remaining, 0)
			issuances = append(issuances, issuanceID)
		}
		var virtualIndex uint32
		for _, prior := range result.inputs {
			if prior.packetIndex == uint32(packet) {
				virtualIndex++
			}
		}
		remaining[packet] += input.Amount
		result.inputs = append(result.inputs, commitInput{
			logicalInputID:    input.ID,
			packetIndex:       uint32(packet),
			virtualInputIndex: virtualIndex,
			anchorOutpoint:    fakeAnchor(input),
			assetRef:          input.AssetRef,
			issuanceID:        issuanceID,
			amount:            input.Amount,
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
	return pieceAt(f.vtxo.Outpoint, id, amount)
}

// pieceAt returns an asset piece in issuance id of the VTXO at outpoint. Its
// confirmed proof names the issuance and the VTXO.
func pieceAt(outpoint wire.OutPoint, id byte,
	amount uint64) AssetVTXOClaimInput {

	return AssetVTXOClaimInput{
		Amount: amount,
		ProofPath: &customanchor.ProofPath{
			ConfirmedBaseProof: []byte{
				id,
				outpoint.Hash[0],
			},
			Steps: []customanchor.ProofPathStep{{
				TransitionProof: fakeProof(outpoint),
			}},
		},
		OPTrueWitness: [][]byte{
			{
				0x51,
			},
		},
	}
}

// otherInput returns a second asset VTXO of the sender holding pieces.
func (f *oorFixture) otherInput(t *testing.T,
	pieces ...AssetVTXOClaimInput) OORAssetInput {

	t.Helper()

	return OORAssetInput{
		Checkpoint: oortx.CheckpointInput{
			SpentVTXO: oortx.SpentVTXORef{
				Outpoint: otherVTXO,
				Output:   f.vtxo.Output,
			},
			OwnerLeafScript: []byte{
				txscript.OP_1,
			},
		},
		Pieces:   pieces,
		Verifier: staticProofVerifier{},
	}
}

// otherVTXO is the outpoint of the sender's second asset VTXO.
var otherVTXO = wire.OutPoint{
	Hash: chainhash.Hash{
		0xcc,
	},
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
			Verifier: staticProofVerifier{},
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
		name: "VTXO spent twice",
		mutate: func(_ *oorFixture, req *OORTransferRequest) {
			req.Inputs = append(req.Inputs, req.Inputs[0])
		},
		err: "asset input 1 spends",
	}, {
		name: "no verifier",
		mutate: func(_ *oorFixture, req *OORTransferRequest) {
			req.Inputs[0].Verifier = nil
		},
		err: "asset input 0 verifier is required",
	}, {
		name: "too many asset inputs",
		mutate: func(_ *oorFixture, req *OORTransferRequest) {
			for len(req.Inputs) <= MaxOORAssetInputs {
				req.Inputs = append(req.Inputs, req.Inputs[0])
			}
		},
		err: "at most 8 asset VTXOs",
	}, {
		name: "proof path full",
		mutate: func(_ *oorFixture, req *OORTransferRequest) {
			path := req.Inputs[0].Pieces[0].ProofPath
			path.Steps = make(
				[]customanchor.ProofPathStep,
				customanchor.ProofPathMaxDepth-1,
			)
		},
		err: "unconfirmed steps",
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
		name: "unreadable claim",
		mutate: func(_ *customanchor.Request, result *commitResult) {
			for idx := range result.inputs {
				result.inputs[idx].virtualInputIndex = 1
			}
		},
		err: "not contiguous",
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

// TestBuildOORTransferMerge checks that a transfer merges two asset VTXOs of
// one issuance: each moves into its own checkpoint, the Ark transition merges
// their pieces, and every new asset VTXO extends the path of the transition's
// first virtual input with the other VTXO's path as a co-input path.
func TestBuildOORTransferMerge(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%v", reverse), func(t *testing.T) {
			f := newOORFixture(t)
			req := f.request(t)
			req.Inputs[0].Pieces = []AssetVTXOClaimInput{
				f.piece(0x07, 30),
			}
			req.Inputs = append(
				req.Inputs,
				f.otherInput(
					t, pieceAt(otherVTXO, 0x07, 70),
				),
			)

			// Both VTXOs' carriers return to the sender.
			req.Outputs[0] = f.output(t, f.change, 2_000)
			driver := &fakeOORDriver{reverseMerges: reverse}

			transfer, err := buildOORTransfer(
				t.Context(), driver, req,
			)
			require.NoError(t, err)
			require.Len(t, transfer.Checkpoints, 2)
			ark := transfer.Ark.UnsignedTx
			require.Len(t, ark.TxIn, 3)
			require.NoError(t, arktx.ValidateCanonicalTx(ark))
			require.Len(t, driver.requests, 3)
			require.Len(t, driver.requests[2].Inputs, 2)

			spine, coInput := []byte{0x07, 0xaa}, []byte{0x07, 0xcc}
			if reverse {
				spine, coInput = coInput, spine
			}
			committed, err := driver.DecodePackage(
				transfer.ArkPackage,
			)
			require.NoError(t, err)
			var assets int
			for idx, recipient := range transfer.Recipients {
				if recipient.Asset == nil {
					continue
				}
				assets++
				pieces, err := claimInputsFromCommit(
					committed, wire.OutPoint{
						Hash:  ark.TxHash(),
						Index: uint32(idx),
					},
				)
				require.NoError(t, err)
				require.Len(t, pieces, 1)
				require.Equal(
					t, recipient.Asset.Amount,
					pieces[0].Amount,
				)

				path := pieces[0].ProofPath
				require.Equal(t, spine, path.ConfirmedBaseProof)
				require.Len(t, path.Steps, 1)
				coPaths := path.Steps[0].CoInputPaths
				require.Len(t, coPaths, 1)
				require.Equal(
					t, coInput,
					coPaths[0].ConfirmedBaseProof,
				)
			}
			require.Equal(t, 2, assets)
		})
	}
}

// TestBuildOORTransferMergeLimits checks that a piece whose proof path already
// nests co-input paths as deep as tap-sdk allows can still move alone, but
// cannot be merged, since it could become a co-input path itself.
func TestBuildOORTransferMergeLimits(t *testing.T) {
	f := newOORFixture(t)

	// The piece's last step merged a path that nests two levels deeper.
	deep := pieceAt(otherVTXO, 0x07, 70)
	nested := &customanchor.ProofPath{ConfirmedBaseProof: []byte{0x07}}
	for range customanchor.ProofPathMaxCoPathDepth - 1 {
		nested = &customanchor.ProofPath{
			ConfirmedBaseProof: []byte{
				0x07,
			},
			Steps: []customanchor.ProofPathStep{{
				TransitionProof: []byte("merge"),
				CoInputPaths: []*customanchor.ProofPath{
					nested,
				},
			}},
		}
	}
	deep.ProofPath.Steps[0].CoInputPaths = []*customanchor.ProofPath{nested}
	require.Equal(
		t, customanchor.ProofPathMaxCoPathDepth,
		proofPathNesting(deep.ProofPath),
	)

	alone := f.request(t)
	alone.Inputs[0] = f.otherInput(t, deep)
	alone.AssetOutputs[1].Amount = 10
	_, err := buildOORTransfer(t.Context(), &fakeOORDriver{}, alone)
	require.NoError(t, err)

	merged := f.request(t)
	merged.Inputs[0].Pieces = []AssetVTXOClaimInput{f.piece(0x07, 30)}
	merged.Inputs = append(merged.Inputs, f.otherInput(t, deep))
	merged.Outputs[0] = f.output(t, f.change, 2_000)
	driver := &fakeOORDriver{}
	_, err = buildOORTransfer(t.Context(), driver, merged)
	require.ErrorIs(t, err, ErrOORProofLimit)
	require.ErrorContains(t, err, "nests 3 co-input levels")

	// Both checkpoints were committed, but no Ark transition.
	require.Len(t, driver.requests, 2)
}

// TestCheckProofPathBounds checks the proof path bounds a new asset VTXO must
// meet: tap-sdk's step count, co-input count per step, and co-input nesting.
// tap-sdk checks a path's shape before it decodes any proof, so its own
// Validate refuses the same paths for the same reason.
func TestCheckProofPathBounds(t *testing.T) {
	t.Parallel()

	leaf := func() *customanchor.ProofPath {
		return &customanchor.ProofPath{ConfirmedBaseProof: []byte{0x07}}
	}
	merge := func(
		coPaths ...*customanchor.ProofPath) *customanchor.ProofPath {

		path := leaf()
		path.Steps = []customanchor.ProofPathStep{{
			TransitionProof: []byte("merge"),
			CoInputPaths:    coPaths,
		}}

		return path
	}
	nest := func(levels int) *customanchor.ProofPath {
		path := leaf()
		for range levels {
			path = merge(path)
		}

		return path
	}
	coPaths := func(count int) []*customanchor.ProofPath {
		paths := make([]*customanchor.ProofPath, count)
		for idx := range paths {
			paths[idx] = leaf()
		}

		return paths
	}
	deep := leaf()
	deep.Steps = make(
		[]customanchor.ProofPathStep, customanchor.ProofPathMaxDepth+1,
	)

	tests := []struct {
		name   string
		path   *customanchor.ProofPath
		err    string
		sdkErr string
	}{{
		name: "deepest nesting",
		path: nest(customanchor.ProofPathMaxCoPathDepth),
	}, {
		name:   "nesting too deep",
		path:   nest(customanchor.ProofPathMaxCoPathDepth + 1),
		err:    "nest 4 levels",
		sdkErr: "co-input path depth exceeds",
	}, {
		name: "widest merge",
		path: merge(coPaths(customanchor.ProofPathMaxStepCoPaths)...),
	}, {
		name: "merge too wide",
		path: merge(
			coPaths(customanchor.ProofPathMaxStepCoPaths + 1)...,
		),
		err:    "merges 16 co-inputs",
		sdkErr: "has 16 co-input paths",
	}, {
		name:   "too many steps",
		path:   deep,
		err:    "65 unconfirmed steps",
		sdkErr: "path depth 65 exceeds",
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := checkProofPathBounds(test.path, 0)
			sdkErr := test.path.Validate()
			require.ErrorIs(
				t, sdkErr, customanchor.ErrProofPathInvalid,
			)
			if test.err == "" {
				require.NoError(t, err)

				// The fake proofs fail only once decoded.
				require.NotContains(
					t, sdkErr.Error(),
					"exceeds",
				)
				require.NotContains(t, sdkErr.Error(), "limit")

				return
			}
			require.ErrorContains(t, err, test.err)
			require.ErrorContains(t, sdkErr, test.sdkErr)
		})
	}
}

// TestBuildOORTransferPlainOutputExclusionMetadata verifies that planOORArk
// populates BIP-371 Taproot internal key and depth-first tap tree metadata on
// every plain P2TR output of the Ark anchor PSBT so the asset engine can
// generate Taproot exclusion proofs, and rejects plain P2TR outputs that omit
// or mismatch their policy template.
func TestBuildOORTransferPlainOutputExclusionMetadata(t *testing.T) {
	t.Parallel()

	f := newOORFixture(t)
	driver := &fakeOORDriver{}
	req := f.request(t)

	_, err := buildOORTransfer(t.Context(), driver, req)
	require.NoError(t, err)
	require.Len(t, driver.requests, 2)

	arkReq := driver.requests[1]
	arkPkt, err := psbt.NewFromRawBytes(
		bytes.NewReader(arkReq.AnchorPSBT), false,
	)
	require.NoError(t, err)

	assetIndices := make(map[uint32]struct{}, len(arkReq.Outputs))
	for _, out := range arkReq.Outputs {
		assetIndices[out.AnchorOutputIndex] = struct{}{}
	}

	var plainP2TRCount int
	for idx, txOut := range arkPkt.UnsignedTx.TxOut {
		if _, isAsset := assetIndices[uint32(idx)]; isAsset {
			continue
		}
		if !txscript.IsPayToTaproot(txOut.PkScript) {
			require.Empty(t, arkPkt.Outputs[idx].TaprootInternalKey)
			require.Empty(t, arkPkt.Outputs[idx].TaprootTapTree)
			continue
		}

		plainP2TRCount++
		pOut := arkPkt.Outputs[idx]
		require.Len(t, pOut.TaprootInternalKey, schnorr.PubKeyBytesLen)
		require.NotEmpty(t, pOut.TaprootTapTree)

		internalKey, err := schnorr.ParsePubKey(pOut.TaprootInternalKey)
		require.NoError(t, err)

		rootHash := parseTestBIP371TapTreeRoot(t, pOut.TaprootTapTree)
		outputKey := txscript.ComputeTaprootOutputKey(
			internalKey, rootHash[:],
		)
		wantScript, err := txscript.PayToTaprootScript(outputKey)
		require.NoError(t, err)
		require.Equal(t, txOut.PkScript, wantScript)
	}
	require.Equal(t, len(req.Outputs), plainP2TRCount)

	// Missing policy template on a plain P2TR output fails before any
	// checkpoint commit.
	missingPolicy := f.request(t)
	missingPolicy.Outputs[0].VTXOPolicyTemplate = nil
	missingDriver := &fakeOORDriver{}
	_, err = buildOORTransfer(t.Context(), missingDriver, missingPolicy)
	require.ErrorContains(t, err, "requires a policy template")
	require.Empty(t, missingDriver.requests)

	// Mismatched policy template on a plain P2TR output fails before any
	// checkpoint commit.
	wrongPolicy := f.request(t)
	wrongPolicy.Outputs[0].VTXOPolicyTemplate =
		wrongPolicy.Outputs[1].VTXOPolicyTemplate
	wrongDriver := &fakeOORDriver{}
	_, err = buildOORTransfer(t.Context(), wrongDriver, wrongPolicy)
	require.ErrorContains(t, err, "does not match pkScript")
	require.Empty(t, wrongDriver.requests)

	// Non-P2TR plain outputs (e.g. P2WPKH) do not require a policy
	// template and receive no Taproot exclusion metadata.
	nonTaproot := f.request(t)
	p2wpkh := append([]byte{txscript.OP_0, 0x14}, make([]byte, 20)...)
	nonTaproot.Outputs[0] = oortx.RecipientOutput{
		PkScript: p2wpkh,
		Value:    1_000,
	}
	nonTaprootDriver := &fakeOORDriver{}
	_, err = buildOORTransfer(t.Context(), nonTaprootDriver, nonTaproot)
	require.NoError(t, err)
	nonTaprootPkt, err := psbt.NewFromRawBytes(
		bytes.NewReader(nonTaprootDriver.requests[1].AnchorPSBT), false,
	)
	require.NoError(t, err)
	var sawP2WPKH bool
	for idx, txOut := range nonTaprootPkt.UnsignedTx.TxOut {
		if !bytes.Equal(txOut.PkScript, p2wpkh) {
			continue
		}
		sawP2WPKH = true
		require.Empty(t, nonTaprootPkt.Outputs[idx].TaprootInternalKey)
		require.Empty(t, nonTaprootPkt.Outputs[idx].TaprootTapTree)
	}
	require.True(t, sawP2WPKH)
}

// TestEncodeBIP371TapTreeShapes verifies that encodeBIP371TapTree reproduces
// arkscript.BuildTree's root hash for 1, 2, 3, and 5 leaves.
func TestEncodeBIP371TapTreeShapes(t *testing.T) {
	t.Parallel()

	for _, leafCount := range []int{1, 2, 3, 5} {
		leaves := make([]arkscript.PolicyLeaf, leafCount)
		for i := range leaves {
			leaves[i] = arkscript.PolicyLeaf{
				Leaf: txscript.NewBaseTapLeaf(
					[]byte{txscript.OP_1 + byte(i)},
				),
			}
		}
		compiled, err := arkscript.BuildTree(
			leaves, &arkscript.ARKNUMSKey,
		)
		require.NoError(t, err)

		encoded, err := encodeBIP371TapTree(compiled)
		require.NoError(t, err)

		gotRoot := parseTestBIP371TapTreeRoot(t, encoded)
		require.Equal(
			t, compiled.RootHash, gotRoot[:], "leafCount=%d",
			leafCount,
		)
	}
}

// parseTestBIP371TapTreeRoot decodes a BIP-371 PSBT_OUT_TAP_TREE blob using
// the exact depth-first tuple grammar expected by Taproot exclusion proof
// builders and returns the reconstructed Taproot merkle root.
func parseTestBIP371TapTreeRoot(t *testing.T, encoded []byte) chainhash.Hash {
	t.Helper()

	type bip371Leaf struct {
		depth uint8
		leaf  txscript.TapLeaf
	}

	r := bytes.NewReader(encoded)
	var leaves []bip371Leaf
	for r.Len() > 0 {
		depth, err := r.ReadByte()
		require.NoError(t, err)
		version, err := r.ReadByte()
		require.NoError(t, err)
		scriptLen, err := wire.ReadVarInt(r, 0)
		require.NoError(t, err)
		script := make([]byte, int(scriptLen))
		_, err = io.ReadFull(r, script)
		require.NoError(t, err)

		leaves = append(leaves, bip371Leaf{
			depth: depth,
			leaf: txscript.NewTapLeaf(
				txscript.TapscriptLeafVersion(version), script,
			),
		})
	}
	require.NotEmpty(t, leaves)

	next := 0
	var parseNode func(depth uint8) txscript.TapNode
	parseNode = func(depth uint8) txscript.TapNode {
		require.Less(t, next, len(leaves))
		if leaves[next].depth == depth {
			leaf := leaves[next].leaf
			next++

			return leaf
		}
		left := parseNode(depth + 1)
		right := parseNode(depth + 1)

		return txscript.NewTapBranch(left, right)
	}

	root := parseNode(0)
	require.Equal(t, len(leaves), next)

	return root.TapHash()
}
