package tapassets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/tap-sdk/customanchor"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
)

const (
	// maxOORAssetOutputs bounds the new asset VTXOs of one transfer: a
	// recipient and the sender's asset change.
	maxOORAssetOutputs = 2

	// maxOOROrderingNonces bounds the key domains tried for an Ark
	// transaction whose asset outputs sort canonically.
	maxOOROrderingNonces = 64

	// maxOOROrderingRounds bounds the reorderings tried per key domain.
	maxOOROrderingRounds = 4
)

// ErrOOROrderingUnresolved means no tried key domain gave an Ark transaction
// whose composed asset outputs sort into the order they were committed in.
var ErrOOROrderingUnresolved = errors.New("asset OOR outputs do not converge " +
	"to a canonical order")

// OORAssetInput is one asset VTXO an OOR transfer spends.
type OORAssetInput struct {
	// Checkpoint names the VTXO and its collaborative leaf, which the
	// checkpoint output commits to.
	Checkpoint oortx.CheckpointInput

	// Pieces are the VTXO's asset pieces, one per issuance, as
	// DecodeAssetVTXOClaimInputs reads them from the VTXO's sealed
	// package.
	Pieces []AssetVTXOClaimInput

	// Verifier verifies the pieces' proof paths.
	Verifier customanchor.ConfirmedProofVerifier
}

// OORAssetOutput is one asset VTXO an OOR transfer creates.
type OORAssetOutput struct {
	// PolicyTemplate is the encoded Ark policy of the new VTXO.
	PolicyTemplate []byte

	// Amount is the number of asset units the VTXO receives.
	Amount uint64

	// Value is the VTXO's Bitcoin carrier.
	Value btcutil.Amount
}

// OORTransferRequest describes the asset transitions of one OOR transfer.
type OORTransferRequest struct {
	// AssetRef names the asset the transfer moves.
	AssetRef tapsdk.AssetRef

	// Digest scopes the OP_TRUE script keys of the transfer's asset
	// outputs. Distinct transfers use distinct digests.
	Digest tapsdk.Hash

	// CheckpointPolicy is the operator's checkpoint policy.
	CheckpointPolicy arkscript.CheckpointPolicy

	// Inputs are the asset VTXOs the transfer spends. A transfer spends
	// one asset VTXO.
	Inputs []OORAssetInput

	// Checkpoints are the transfer's checkpoint outputs that carry no
	// asset, such as a Bitcoin VTXO or a leased carrier.
	Checkpoints []oortx.CheckpointOutput

	// AssetOutputs are the asset VTXOs the transfer creates. Their
	// amounts consume the inputs' asset amount exactly.
	AssetOutputs []OORAssetOutput

	// Outputs are the transfer's outputs that carry no asset.
	Outputs []oortx.RecipientOutput
}

// OORTransfer is the asset side of one OOR transfer: the checkpoint and Ark
// transactions with their sealed asset transitions.
type OORTransfer struct {
	// Checkpoints are the checkpoint transactions of the asset inputs, in
	// request order. Their outputs commit to the asset.
	Checkpoints []*oortx.CheckpointArtifact

	// CheckpointAssetRoots are the asset roots the checkpoint outputs
	// commit to, in request order.
	CheckpointAssetRoots []chainhash.Hash

	// CheckpointPackages are the sealed checkpoint transitions, in request
	// order.
	CheckpointPackages [][]byte

	// Ark is the unsigned Ark transaction.
	Ark *psbt.Packet

	// ArkPackage is the sealed Ark transition. DecodeAssetVTXOClaimInputs
	// reads every new asset VTXO's pieces from it.
	ArkPackage []byte

	// Recipients are the Ark transaction's outputs before its anchor, asset
	// outputs with their asset.
	Recipients []oortx.RecipientOutput
}

// oorTransferDriver is the tap-sdk seam of an OOR transfer.
type oorTransferDriver interface {
	assetTreeDriver

	Preview(context.Context, *customanchor.Request,
		customanchor.ConfirmedProofVerifier) (
		[]outputCommitmentPreview,
		error,
	)
}

// BuildOORTransfer commits the asset transitions of an OOR transfer: one per
// asset input from its VTXO to its checkpoint output, then one from the
// checkpoint outputs to the new asset VTXOs. The caller signs and submits the
// transactions, and keeps the result until the transfer settles.
func BuildOORTransfer(ctx context.Context, wallet *tapsdk.Wallet,
	req OORTransferRequest) (*OORTransfer, error) {

	return buildOORTransfer(ctx, &sdkDriver{wallet: wallet}, req)
}

func buildOORTransfer(ctx context.Context, driver oorTransferDriver,
	req OORTransferRequest) (*OORTransfer, error) {

	if err := req.validate(); err != nil {
		return nil, err
	}

	result := &OORTransfer{}
	checkpoints := make(
		[]oortx.CheckpointOutput, 0,
		len(req.Inputs)+len(req.Checkpoints),
	)
	var sources []*assetSpendSource
	for idx := range req.Inputs {
		checkpoint, err := commitOORCheckpoint(
			ctx, driver, req, &req.Inputs[idx],
		)
		if err != nil {
			return nil, fmt.Errorf("asset input %d checkpoint: %w",
				idx, err)
		}

		result.Checkpoints = append(
			result.Checkpoints, checkpoint.artifact,
		)
		result.CheckpointAssetRoots = append(
			result.CheckpointAssetRoots, checkpoint.assetRoot,
		)
		result.CheckpointPackages = append(
			result.CheckpointPackages, checkpoint.packageBytes,
		)
		checkpoints = append(checkpoints, checkpoint.output)
		sources = append(sources, checkpoint.sources...)
	}
	checkpoints = append(checkpoints, req.Checkpoints...)

	ark, err := commitOORArk(ctx, driver, req, checkpoints, sources)
	if err != nil {
		return nil, fmt.Errorf("asset Ark transition: %w", err)
	}
	result.Ark = ark.packet
	result.ArkPackage = ark.packageBytes
	result.Recipients = ark.recipients

	return result, nil
}

// validate checks the request before anything is committed.
func (r *OORTransferRequest) validate() error {
	switch {
	case r.AssetRef == "":
		return fmt.Errorf("asset ref is required")

	case r.Digest == (tapsdk.Hash{}):
		return fmt.Errorf("transfer digest is required")

	case len(r.Inputs) != 1:
		return fmt.Errorf("a transfer spends one asset VTXO, got %d",
			len(r.Inputs))

	case len(r.AssetOutputs) == 0:
		return fmt.Errorf("asset outputs are required")

	case len(r.AssetOutputs) > maxOORAssetOutputs:
		return fmt.Errorf("a transfer creates at most %d asset "+
			"outputs, got %d", maxOORAssetOutputs,
			len(r.AssetOutputs))
	}

	var inputTotal uint64
	for idx := range r.Inputs {
		input := &r.Inputs[idx]
		if len(input.Pieces) == 0 {
			return fmt.Errorf("asset input %d holds no asset", idx)
		}
		if len(input.Pieces) > MaxTransitionInputs {
			return fmt.Errorf("%w: asset input %d holds %d pieces",
				ErrTooManyTransitionInputs, idx,
				len(input.Pieces))
		}
		for pieceIdx := range input.Pieces {
			piece := &input.Pieces[pieceIdx]
			if piece.Amount == 0 || piece.ProofPath == nil ||
				len(piece.OPTrueWitness) == 0 {
				return fmt.Errorf("asset input %d piece %d is "+
					"incomplete", idx, pieceIdx)
			}
			if piece.Amount > math.MaxUint64-inputTotal {
				return fmt.Errorf("asset input amount overflow")
			}
			inputTotal += piece.Amount
		}
	}

	var outputTotal uint64
	for idx := range r.AssetOutputs {
		output := &r.AssetOutputs[idx]
		switch {
		case output.Amount == 0:
			return fmt.Errorf("asset output %d amount must be "+
				"positive", idx)

		case output.Value <= 0:
			return fmt.Errorf("asset output %d carrier must be "+
				"positive", idx)

		case len(output.PolicyTemplate) == 0:
			return fmt.Errorf("asset output %d policy is required",
				idx)
		}
		if output.Amount > math.MaxUint64-outputTotal {
			return fmt.Errorf("asset output amount overflow")
		}
		outputTotal += output.Amount
	}
	if inputTotal != outputTotal {
		return fmt.Errorf("asset outputs hold %d units, inputs hold %d",
			outputTotal, inputTotal)
	}

	// The Ark transaction locates each output by value and script, so no
	// two outputs may share both. An asset output is located by its
	// uncomposed policy script.
	type outputKey struct {
		value  btcutil.Amount
		script string
	}
	seen := make(map[outputKey]struct{})
	addOutput := func(value btcutil.Amount, script []byte) error {
		key := outputKey{value: value, script: string(script)}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("two outputs pay %d to one script",
				value)
		}
		seen[key] = struct{}{}

		return nil
	}
	for idx := range r.AssetOutputs {
		output := &r.AssetOutputs[idx]
		anchor, err := policyAnchor(output.PolicyTemplate)
		if err != nil {
			return fmt.Errorf("asset output %d: %w", idx, err)
		}
		err = addOutput(output.Value, anchor.UncomposedPkScript)
		if err != nil {
			return err
		}
	}
	for idx := range r.Outputs {
		output := &r.Outputs[idx]
		if output.Asset != nil {
			return fmt.Errorf("output %d carries an asset", idx)
		}
		if err := addOutput(output.Value, output.PkScript); err != nil {
			return err
		}
	}

	return nil
}

// oorCheckpoint is one committed checkpoint transition.
type oorCheckpoint struct {
	artifact     *oortx.CheckpointArtifact
	output       oortx.CheckpointOutput
	assetRoot    chainhash.Hash
	packageBytes []byte
	sources      []*assetSpendSource
}

// commitOORCheckpoint moves an asset VTXO's pieces into its checkpoint output.
func commitOORCheckpoint(ctx context.Context, driver oorTransferDriver,
	req OORTransferRequest, input *OORAssetInput) (*oorCheckpoint, error) {

	template, err := oortx.BuildCheckpointPSBT(
		req.CheckpointPolicy, input.Checkpoint,
	)
	if err != nil {
		return nil, err
	}
	tapscript, err := arkscript.CheckpointTapScript(
		req.CheckpointPolicy, template.OwnerLeafScript,
	)
	if err != nil {
		return nil, err
	}
	anchor, err := anchorPlan(&arkscript.ARKNUMSKey, tapscript.Leaves)
	if err != nil {
		return nil, err
	}

	vtxo := input.Checkpoint.SpentVTXO
	sources := make([]*assetSpendSource, len(input.Pieces))
	assetInputs := make([]customanchor.AssetInput, len(input.Pieces))
	var amount uint64
	for idx := range input.Pieces {
		piece := &input.Pieces[idx]
		sources[idx] = &assetSpendSource{
			proofPath: piece.ProofPath.Clone(),
			witness: customanchor.AssetWitnessPlan{
				Mode:  witnessCallerProvided,
				Stack: cloneByteSlices(piece.OPTrueWitness),
			},
			verifier:       input.Verifier,
			amount:         piece.Amount,
			anchorOutpoint: sdkOutpoint(vtxo.Outpoint),
		}
		assetInputs[idx], err = sources[idx].customInput(
			oorCheckpointInputID(idx), req.AssetRef,
		)
		if err != nil {
			return nil, err
		}
		amount += piece.Amount
	}
	if err := validateOORPathCapacity(sources); err != nil {
		return nil, err
	}

	anchorBytes, err := psbtutil.Serialize(template.PSBT)
	if err != nil {
		return nil, err
	}
	value := template.PSBT.UnsignedTx.TxOut[0].Value
	request := &customanchor.Request{
		Inputs: assetInputs,
		Outputs: []customanchor.AssetOutput{{
			ID:                oorCheckpointOutputID,
			AssetRef:          req.AssetRef,
			Amount:            amount,
			AnchorOutputIndex: 0,
			AnchorValueSat:    uint64(value),
			Script: opTrueScript(
				req.Digest, fmt.Sprintf("oor/checkpoint/%s",
					vtxo.Outpoint),
			),
			Anchor: anchor,
		}},
		AnchorPSBT:    anchorBytes,
		Funding:       callerFundedExact(),
		PassiveAssets: passiveReject(),
		LossPolicy:    lossReject(),
		SigningPlans:  callerSignedPlans(1),
	}

	committed, err := driver.Commit(ctx, request, &assetSourceVerifier{
		sources: sources,
	})
	if err != nil {
		return nil, err
	}

	specs := []oorOutputSpec{{
		index:       0,
		logicalID:   oorCheckpointOutputID,
		amount:      amount,
		value:       value,
		internalKey: &arkscript.ARKNUMSKey,
		leaves:      tapscript.Leaves,
	}}
	inputIDs := make([]string, len(sources))
	for idx := range sources {
		inputIDs[idx] = oorCheckpointInputID(idx)
	}
	committedTx, roots, err := validateOORCommit(
		committed, req.AssetRef, template.PSBT.UnsignedTx, sources,
		inputIDs, specs,
	)
	if err != nil {
		return nil, err
	}

	artifact := *template
	artifact.PSBT, err = psbtutil.Parse(committed.anchorPSBT)
	if err != nil {
		return nil, err
	}
	artifact.PSBT.Inputs[0].WitnessUtxo = vtxo.Output
	artifact.PSBT.Outputs[0].TaprootTapTree = bytes.Clone(
		template.TapTreeEncoded,
	)
	output, err := artifact.ToCheckpointOutput()
	if err != nil {
		return nil, err
	}

	childSources, err := oorChildSources(
		sources, committed, committedTx, 0,
	)
	if err != nil {
		return nil, err
	}

	return &oorCheckpoint{
		artifact:     &artifact,
		output:       output,
		assetRoot:    roots[0],
		packageBytes: bytes.Clone(committed.packageBytes),
		sources:      childSources,
	}, nil
}

// oorArk is the committed Ark transition.
type oorArk struct {
	packet       *psbt.Packet
	packageBytes []byte
	recipients   []oortx.RecipientOutput
}

// oorArkOutput is one asset output of the Ark transaction.
type oorArkOutput struct {
	request     *OORAssetOutput
	routing     []byte
	internalKey *btcec.PublicKey
	leaves      []txscript.TapLeaf
}

// commitOORArk moves the checkpoint outputs' asset into the new asset VTXOs.
//
// The Ark transaction orders outputs by value and script, and an asset
// output's script commits to an asset root that depends on the output order
// through the split commitment. A key domain is therefore accepted only once
// the previewed scripts sort into the order they were previewed in.
func commitOORArk(ctx context.Context, driver oorTransferDriver,
	req OORTransferRequest, checkpoints []oortx.CheckpointOutput,
	sources []*assetSpendSource) (*oorArk, error) {

	outputs := make([]oorArkOutput, len(req.AssetOutputs))
	for idx := range req.AssetOutputs {
		output := &req.AssetOutputs[idx]
		anchor, err := policyAnchor(output.PolicyTemplate)
		if err != nil {
			return nil, fmt.Errorf("asset output %d: %w", idx, err)
		}
		outputs[idx] = oorArkOutput{
			request:     output,
			routing:     anchor.UncomposedPkScript,
			internalKey: anchor.InternalKey,
			leaves:      anchor.TapLeaves,
		}
	}

	assetInputs := make([]customanchor.AssetInput, len(sources))
	for idx := range sources {
		assetInput, err := sources[idx].customInput(
			oorArkInputID(idx), req.AssetRef,
		)
		if err != nil {
			return nil, err
		}
		assetInputs[idx] = assetInput
	}
	verifier := &assetSourceVerifier{sources: sources}

	for nonce := range maxOOROrderingNonces {
		scripts := make([][]byte, len(outputs))
		for idx := range outputs {
			scripts[idx] = outputs[idx].routing
		}

		for range maxOOROrderingRounds {
			plan, err := planOORArk(
				req, nonce, checkpoints, outputs, scripts,
				assetInputs,
			)
			if err != nil {
				return nil, err
			}
			previews, err := driver.Preview(
				ctx, plan.request, verifier,
			)
			if err != nil {
				return nil, err
			}
			composed, err := plan.composedScripts(previews)
			if err != nil {
				return nil, err
			}
			if !sameScripts(composed, scripts) {
				scripts = composed
				continue
			}

			return commitPlannedOORArk(
				ctx, driver, req, plan, sources, verifier,
			)
		}
	}

	return nil, ErrOOROrderingUnresolved
}

// oorArkPlan is one candidate Ark transition.
type oorArkPlan struct {
	request    *customanchor.Request
	template   *psbt.Packet
	recipients []oortx.RecipientOutput
	specs      []oorOutputSpec
	outputs    []oorArkOutput
}

// planOORArk lays out the Ark transaction with asset outputs sorted under the
// given scripts, and hands tap-sdk their uncomposed scripts.
func planOORArk(req OORTransferRequest, nonce int,
	checkpoints []oortx.CheckpointOutput, outputs []oorArkOutput,
	scripts [][]byte,
	assetInputs []customanchor.AssetInput) (*oorArkPlan, error) {

	recipients := make(
		[]oortx.RecipientOutput, 0, len(outputs)+len(req.Outputs),
	)
	for idx := range outputs {
		recipients = append(recipients, oortx.RecipientOutput{
			PkScript: bytes.Clone(scripts[idx]),
			Value:    outputs[idx].request.Value,
		})
	}
	recipients = append(recipients, req.Outputs...)

	template, err := oortx.BuildArkPSBT(checkpoints, recipients)
	if err != nil {
		return nil, err
	}

	plan := &oorArkPlan{
		template:   template,
		recipients: oortx.CanonicalRecipientOutputs(recipients),
		outputs:    outputs,
	}
	assetOutputs := make([]customanchor.AssetOutput, len(outputs))
	for idx := range outputs {
		output := &outputs[idx]
		index, err := oortx.RecipientOutputIndex(
			recipients, recipients[idx],
		)
		if err != nil {
			return nil, fmt.Errorf("asset output %d: %w", idx, err)
		}
		template.UnsignedTx.TxOut[index].PkScript = bytes.Clone(
			output.routing,
		)

		anchor, err := anchorPlan(output.internalKey, output.leaves)
		if err != nil {
			return nil, fmt.Errorf("asset output %d: %w", idx, err)
		}
		logicalID := oorArkOutputID(idx)
		assetOutputs[idx] = customanchor.AssetOutput{
			ID:                logicalID,
			AssetRef:          req.AssetRef,
			Amount:            output.request.Amount,
			AnchorOutputIndex: index,
			AnchorValueSat:    uint64(output.request.Value),
			Script: opTrueScript(
				req.Digest, fmt.Sprintf("oor/ark/%d/%d", nonce,
					idx),
			),
			Anchor: anchor,
		}
		plan.specs = append(plan.specs, oorOutputSpec{
			index:       index,
			logicalID:   logicalID,
			amount:      output.request.Amount,
			value:       int64(output.request.Value),
			internalKey: output.internalKey,
			leaves:      output.leaves,
		})
	}

	anchorBytes, err := psbtutil.Serialize(template)
	if err != nil {
		return nil, err
	}
	plan.request = &customanchor.Request{
		Inputs:        assetInputs,
		Outputs:       assetOutputs,
		AnchorPSBT:    anchorBytes,
		Funding:       callerFundedExact(),
		PassiveAssets: passiveReject(),
		LossPolicy:    lossReject(),
		SigningPlans: callerSignedPlans(
			len(template.UnsignedTx.TxIn),
		),
	}

	return plan, nil
}

// composedScripts returns the scripts the previewed roots give the asset
// outputs, in request order.
func (p *oorArkPlan) composedScripts(previews []outputCommitmentPreview) (
	[][]byte, error) {

	scripts := make([][]byte, len(p.specs))
	for idx := range p.specs {
		spec := &p.specs[idx]
		preview, err := previewedOutput(
			previews, spec.logicalID, spec.index,
		)
		if err != nil {
			return nil, err
		}
		scripts[idx], err = composedScript(
			spec.internalKey, preview.merkleRoot,
		)
		if err != nil {
			return nil, err
		}
	}

	return scripts, nil
}

// commitPlannedOORArk commits an Ark transition whose previewed scripts sort
// into its layout, and fills in the asset outputs.
func commitPlannedOORArk(ctx context.Context, driver oorTransferDriver,
	req OORTransferRequest, plan *oorArkPlan, sources []*assetSpendSource,
	verifier customanchor.ConfirmedProofVerifier) (*oorArk, error) {

	committed, err := driver.Commit(ctx, plan.request, verifier)
	if err != nil {
		return nil, err
	}

	inputIDs := make([]string, len(sources))
	for idx := range sources {
		inputIDs[idx] = oorArkInputID(idx)
	}
	committedTx, roots, err := validateOORCommit(
		committed, req.AssetRef, plan.template.UnsignedTx, sources,
		inputIDs, plan.specs,
	)
	if err != nil {
		return nil, err
	}

	recipients := plan.recipients
	for idx := range plan.specs {
		spec := &plan.specs[idx]
		recipient := &recipients[spec.index]
		recipient.PkScript = bytes.Clone(
			committedTx.TxOut[spec.index].PkScript,
		)
		recipient.VTXOPolicyTemplate = bytes.Clone(
			plan.outputs[idx].request.PolicyTemplate,
		)
		recipient.Asset = &oortx.RecipientAsset{
			Root:   roots[idx],
			Ref:    string(req.AssetRef),
			Amount: spec.amount,
		}
	}

	var checkpoints []oortx.CheckpointOutput
	for idx := range plan.template.UnsignedTx.TxIn {
		checkpoints = append(checkpoints, oortx.CheckpointOutput{
			Txid: plan.template.UnsignedTx.TxIn[idx].
				PreviousOutPoint.Hash,
			Output: plan.template.Inputs[idx].WitnessUtxo,
		})
	}
	ark, err := oortx.BuildArkPSBT(checkpoints, recipients)
	if err != nil {
		return nil, fmt.Errorf("committed Ark outputs are not "+
			"canonical: %w", err)
	}
	if ark.UnsignedTx.TxHash() != committedTx.TxHash() {
		return nil, fmt.Errorf("committed Ark transaction is not " +
			"canonical")
	}

	return &oorArk{
		packet:       ark,
		packageBytes: bytes.Clone(committed.packageBytes),
		recipients:   recipients,
	}, nil
}

// oorOutputSpec describes one asset output a transition must commit.
type oorOutputSpec struct {
	index       uint32
	logicalID   string
	amount      uint64
	value       int64
	internalKey *btcec.PublicKey
	leaves      []txscript.TapLeaf
}

// validateOORCommit checks a committed transition against its template and
// returns the committed transaction with each spec's asset root.
func validateOORCommit(committed *commitResult, assetRef tapsdk.AssetRef,
	template *wire.MsgTx, sources []*assetSpendSource, inputIDs []string,
	specs []oorOutputSpec) (*wire.MsgTx, []chainhash.Hash, error) {

	switch {
	case committed == nil:
		return nil, nil, fmt.Errorf("asset commit result is required")

	case committed.fundingMode != customanchor.FundingCallerFundedExact:
		return nil, nil, fmt.Errorf("unexpected funding mode %d",
			committed.fundingMode)

	case committed.actualFeeSat != 0:
		return nil, nil, fmt.Errorf("OOR transactions pay no "+
			"fee, got %d", committed.actualFeeSat)
	}

	if err := validateOORCommitInputs(
		committed.inputs, assetRef, sources, inputIDs,
	); err != nil {
		return nil, nil, err
	}
	if err := validateIssuanceConservation(
		sources, committed.outputs,
	); err != nil {
		return nil, nil, err
	}

	packet, err := psbtutil.Parse(committed.anchorPSBT)
	if err != nil {
		return nil, nil, err
	}
	committedTx := packet.UnsignedTx
	expected := template.Copy()

	var specOutputs int
	roots := make([]chainhash.Hash, len(specs))
	for idx := range specs {
		spec := &specs[idx]
		var outputs []*commitOutput
		for outIdx := range committed.outputs {
			output := &committed.outputs[outIdx]
			if output.logicalOutputID == spec.logicalID {
				outputs = append(outputs, output)
			}
		}
		if len(outputs) == 0 {
			return nil, nil, fmt.Errorf("asset commit misses "+
				"output %q", spec.logicalID)
		}
		specOutputs += len(outputs)

		if int(spec.index) >= len(committedTx.TxOut) {
			return nil, nil, fmt.Errorf("committed transaction "+
				"misses output %d", spec.index)
		}
		if err := validateOOROutputs(
			assetRef, committedTx, spec, outputs,
		); err != nil {
			return nil, nil, err
		}

		script, err := composedScript(
			spec.internalKey, outputs[0].taprootMerkleRoot,
		)
		if err != nil {
			return nil, nil, err
		}
		roots[idx] = chainhash.Hash(outputs[0].taprootAssetRoot)
		policyScript, err := composedPolicyScript(
			spec.internalKey, spec.leaves, roots[idx],
		)
		if err != nil {
			return nil, nil, err
		}
		if !bytes.Equal(script, policyScript) ||
			!bytes.Equal(
				committedTx.TxOut[spec.index].PkScript, script,
			) {
			return nil, nil, fmt.Errorf("output %d does not "+
				"commit to its policy and asset root",
				spec.index)
		}
		expected.TxOut[spec.index].PkScript = script
	}
	if specOutputs != len(committed.outputs) {
		return nil, nil, fmt.Errorf("asset commit has %d "+
			"outputs, want %d", len(committed.outputs), specOutputs)
	}

	var expectedBuf, committedBuf bytes.Buffer
	if err := expected.Serialize(&expectedBuf); err != nil {
		return nil, nil, err
	}
	if err := committedTx.Serialize(&committedBuf); err != nil {
		return nil, nil, err
	}
	if !bytes.Equal(expectedBuf.Bytes(), committedBuf.Bytes()) {
		return nil, nil, fmt.Errorf("committed transaction diverges " +
			"from its template")
	}

	return committedTx, roots, nil
}

// validateOORCommitInputs checks the committed asset inputs against their
// sources and records each source's issuance.
func validateOORCommitInputs(committed []commitInput, assetRef tapsdk.AssetRef,
	sources []*assetSpendSource, inputIDs []string) error {

	if len(committed) != len(sources) {
		return fmt.Errorf("asset commit has %d inputs, want %d",
			len(committed), len(sources))
	}
	byID := make(map[string]*commitInput, len(committed))
	for idx := range committed {
		assetInput := &committed[idx]
		if _, ok := byID[assetInput.logicalInputID]; ok {
			return fmt.Errorf("asset commit repeats input %q",
				assetInput.logicalInputID)
		}
		byID[assetInput.logicalInputID] = assetInput
	}

	for idx, source := range sources {
		assetInput, ok := byID[inputIDs[idx]]
		if !ok {
			return fmt.Errorf("asset commit misses input %q",
				inputIDs[idx])
		}
		if assetInput.anchorOutpoint != source.anchorOutpoint {
			return fmt.Errorf("asset input %d outpoint mismatch",
				idx)
		}
		if !assetInput.assetRef.Equivalent(assetRef) {
			return fmt.Errorf("asset input %d ref mismatch", idx)
		}
		if assetInput.amount != source.amount {
			return fmt.Errorf("asset input %d amount %d, want %d",
				idx, assetInput.amount, source.amount)
		}
		if source.issuanceKnown &&
			assetInput.issuanceID != source.issuanceID {
			return fmt.Errorf("asset input %d issuance mismatch",
				idx)
		}
		source.issuanceID = assetInput.issuanceID
		source.issuanceKnown = true
	}

	seen := make(map[tapsdk.AssetID]struct{}, len(sources))
	for _, source := range sources {
		if _, ok := seen[source.issuanceID]; ok {
			return fmt.Errorf("asset inputs repeat an issuance")
		}
		seen[source.issuanceID] = struct{}{}
	}

	return nil
}

// validateOOROutputs checks the pieces committed to one asset output.
func validateOOROutputs(assetRef tapsdk.AssetRef, committedTx *wire.MsgTx,
	spec *oorOutputSpec, outputs []*commitOutput) error {

	wantOutpoint := sdkOutpoint(wire.OutPoint{
		Hash:  committedTx.TxHash(),
		Index: spec.index,
	})
	issuances := make(map[tapsdk.AssetID]struct{}, len(outputs))
	var total uint64
	for _, output := range outputs {
		switch {
		case output.anchorOutputIndex != spec.index:
			return fmt.Errorf("output %q anchored at %d, want %d",
				spec.logicalID, output.anchorOutputIndex,
				spec.index)

		case output.anchorOutpoint != wantOutpoint:
			return fmt.Errorf("output %d anchor outpoint mismatch",
				spec.index)

		case output.anchorValueSat != spec.value:
			return fmt.Errorf("output %d carrier %d, want %d",
				spec.index, output.anchorValueSat, spec.value)

		case !output.assetRef.Equivalent(assetRef):
			return fmt.Errorf("output %d asset ref mismatch",
				spec.index)

		case output.scriptMode != customanchor.AssetScriptOPTrue:
			return fmt.Errorf("output %d script mode %d is "+
				"not OP_TRUE", spec.index, output.scriptMode)

		case len(output.opTrueWitness) == 0 ||
			len(output.proofBlob) == 0:
			return fmt.Errorf("output %d misses witness or proof",
				spec.index)

		case output.taprootAssetRoot != outputs[0].taprootAssetRoot ||
			output.taprootMerkleRoot !=
				outputs[0].taprootMerkleRoot:
			return fmt.Errorf("output %d has inconsistent roots",
				spec.index)

		case output.taprootAssetRoot == (tapsdk.Hash{}):
			return fmt.Errorf("output %d misses its asset root",
				spec.index)
		}
		if _, ok := issuances[output.issuanceID]; ok {
			return fmt.Errorf("output %d repeats an issuance",
				spec.index)
		}
		issuances[output.issuanceID] = struct{}{}
		if output.amount > math.MaxUint64-total {
			return fmt.Errorf("output %d amount overflow",
				spec.index)
		}
		total += output.amount
	}
	if total != spec.amount {
		return fmt.Errorf("output %d holds %d units, want %d",
			spec.index, total, spec.amount)
	}

	return nil
}

// oorChildSources extends each source's proof path with the transition that
// moved its issuance into the output at index.
func oorChildSources(sources []*assetSpendSource, committed *commitResult,
	committedTx *wire.MsgTx, index uint32) ([]*assetSpendSource, error) {

	byIssuance := make(map[tapsdk.AssetID]*assetSpendSource, len(sources))
	for _, source := range sources {
		byIssuance[source.issuanceID] = source
	}

	serializedTx := serializeTx(committedTx)
	var children []*assetSpendSource
	for idx := range committed.outputs {
		output := committed.outputs[idx]
		if output.anchorOutputIndex != index {
			continue
		}
		source := byIssuance[output.issuanceID]
		if source == nil {
			return nil, fmt.Errorf("output %d has an unknown "+
				"issuance", index)
		}
		child, err := source.appendTransition(output, serializedTx)
		if err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	if len(children) != len(sources) {
		return nil, fmt.Errorf("output %d holds %d issuances, want %d",
			index, len(children), len(sources))
	}

	return children, nil
}

// validateOORPathCapacity checks that every piece's proof path can take the
// checkpoint and Ark transitions. tap-sdk enforces the exact bounds; this
// refuses a path that has no room before anything is committed.
func validateOORPathCapacity(sources []*assetSpendSource) error {
	for idx, source := range sources {
		path := source.proofPath
		depth := len(path.Steps)
		if depth+2 > customanchor.ProofPathMaxDepth {
			return fmt.Errorf("asset piece %d proof depth %d "+
				"cannot take an OOR transfer", idx, depth)
		}

		size := uint64(len(path.ConfirmedBaseProof))
		for _, step := range path.Steps {
			size += uint64(4 + len(step.TransitionProof))
		}
		budget := uint64(2 * (4 + customanchor.ProofPathMaxStepSize))
		if size+budget > customanchor.ProofPathMaxSize {
			return fmt.Errorf("asset piece %d proof path lacks "+
				"space for an OOR transfer", idx)
		}
	}

	return nil
}

// policyAnchor returns the Taproot material of an encoded Ark policy.
func policyAnchor(encoded []byte) (TreeLeafAnchor, error) {
	template, err := arkscript.DecodePolicyTemplate(encoded)
	if err != nil {
		return TreeLeafAnchor{}, fmt.Errorf("decode policy: %w", err)
	}
	policy, err := template.Compile()
	if err != nil {
		return TreeLeafAnchor{}, fmt.Errorf("compile policy: %w", err)
	}
	pkScript, err := template.PkScript()
	if err != nil {
		return TreeLeafAnchor{}, fmt.Errorf("policy script: %w", err)
	}
	leaves := make([]txscript.TapLeaf, len(policy.Leaves))
	for idx := range policy.Leaves {
		leaves[idx] = policy.Leaves[idx].Leaf
	}

	return TreeLeafAnchor{
		UncomposedPkScript: pkScript,
		InternalKey:        policy.InternalKey,
		TapLeaves:          leaves,
	}, nil
}

// composedPolicyScript returns the script of the policy leaves composed with
// an asset root.
func composedPolicyScript(internalKey *btcec.PublicKey,
	leaves []txscript.TapLeaf, assetRoot chainhash.Hash) ([]byte, error) {

	policyLeaves := make([]arkscript.PolicyLeaf, len(leaves))
	for idx := range leaves {
		policyLeaves[idx] = arkscript.PolicyLeaf{Leaf: leaves[idx]}
	}
	policy, err := arkscript.BuildTree(policyLeaves, internalKey)
	if err != nil {
		return nil, err
	}
	composed, err := arkscript.ComposeWithSiblingRoot(policy, assetRoot)
	if err != nil {
		return nil, err
	}

	return txscript.PayToTaprootScript(composed.OutputKey())
}

// opTrueScript locks an asset output under a deterministic OP_TRUE key.
func opTrueScript(digest tapsdk.Hash,
	domain string) customanchor.AssetScriptPlan {

	return customanchor.AssetScriptPlan{
		Mode: customanchor.AssetScriptOPTrue,
		OPTrue: &customanchor.AssetOPTrueScriptPlan{
			InternalKey: tapsdk.KeyDescriptor{
				RawKeyBytes: deterministicKey(digest, domain),
			},
		},
	}
}

// callerSignedPlans declares every anchor input signed by the caller.
func callerSignedPlans(inputs int) []customanchor.InputSigningPlan {
	plans := make([]customanchor.InputSigningPlan, inputs)
	for idx := range plans {
		plans[idx] = customanchor.InputSigningPlan{
			InputIndex:   uint32(idx),
			CallerSigned: &customanchor.CallerSignedPlan{},
		}
	}

	return plans
}

func passiveReject() customanchor.PassiveAssets {
	return customanchor.PassiveAssets{Policy: customanchor.PassiveReject}
}

func lossReject() customanchor.LossPolicy {
	return customanchor.LossPolicy{Mode: customanchor.LossReject}
}

// sameScripts reports whether two script lists are equal.
func sameScripts(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for idx := range a {
		if !bytes.Equal(a[idx], b[idx]) {
			return false
		}
	}

	return true
}

const oorCheckpointOutputID = "wavelength-oor-checkpoint"

func oorCheckpointInputID(idx int) string {
	return fmt.Sprintf("wavelength-oor-checkpoint-input-%d", idx)
}

func oorArkInputID(idx int) string {
	return fmt.Sprintf("wavelength-oor-ark-input-%d", idx)
}

func oorArkOutputID(idx int) string {
	return fmt.Sprintf("wavelength-oor-ark-output-%d", idx)
}
