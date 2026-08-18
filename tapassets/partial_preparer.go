package tapassets

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/oor"
)

const (
	receiverOutputID = "wavelength-recipient"
	changeOutputID   = "wavelength-asset-change"

	// assetProofPathPackageHeadroom reserves space for package metadata,
	// anchor PSBTs, and bounded framing beyond the two maximum-sized proof
	// steps added by one Wavelength transfer.
	assetProofPathPackageHeadroom = 1 * 1024 * 1024
	assetProofPathFixedOverhead   = 64
)

type assetSpendSource struct {
	proofFile []byte
	proofPath *tapsdk.AssetProofPath
	witness   tapsdk.CustomAssetWitnessPlan
	verifier  tapsdk.ConfirmedProofVerifier
}

// validateTransitionCapacity ensures the source has room for both graph edges
// before Wavelength commits the checkpoint transition. One slot is consumed by
// the checkpoint proof used as the Ark input and one by the sealed Ark output
// proof that a later spend appends while reconstructing its compact path.
func (s *assetSpendSource) validateTransitionCapacity() error {
	if s == nil {
		return fmt.Errorf("Taproot Asset proof source is required")
	}

	depth := 0
	if s.proofPath != nil {
		depth = len(s.proofPath.Steps)
	}
	if depth+2 > tapsdk.AssetProofPathMaxDepth {
		return fmt.Errorf("Taproot Asset proof path depth %d leaves "+
			"no room for both Wavelength transitions", depth)
	}
	baseSize := len(s.proofFile)
	if s.proofPath != nil {
		baseSize = len(s.proofPath.ConfirmedBaseProof)
		for idx := range s.proofPath.Steps {
			baseSize += 4 + len(
				s.proofPath.Steps[idx].TransitionProof,
			)
		}
		baseSize += 4
	}
	if baseSize > tapsdk.AssetProofPathMaxConfirmedProofSize &&
		s.proofPath == nil {
		return fmt.Errorf("Taproot Asset confirmed proof exceeds "+
			"%d bytes", tapsdk.AssetProofPathMaxConfirmedProofSize)
	}
	requiredHeadroom := 2*(4+tapsdk.AssetProofPathMaxStepSize) +
		assetProofPathPackageHeadroom + assetProofPathFixedOverhead
	if baseSize > tapsdk.AssetProofPathMaxSize-requiredHeadroom {
		return fmt.Errorf("Taproot Asset proof path leaves " +
			"insufficient byte headroom for both Wavelength " +
			"transitions")
	}

	return nil
}

// verify performs the complete proof/path validation before the atomic input
// reservation set is acquired. The SDK repeats this during build, but doing it
// here keeps deterministic proof, passive-asset, and lineage failures on the
// safe side of Wavelength's reservation point of no return.
func (s *assetSpendSource) verify(ctx context.Context) error {
	if s == nil || s.verifier == nil {
		return fmt.Errorf("Taproot Asset proof source verifier is " +
			"required")
	}

	if s.proofPath == nil {
		verification, err := s.verifier.VerifyConfirmedProof(
			ctx,
			append(
				[]byte(nil), s.proofFile...,
			),
		)
		if err != nil {
			return fmt.Errorf("preflight Taproot Asset confirmed "+
				"proof: %w", err)
		}
		if verification == nil ||
			!verification.AnchorAssetInventoryComplete {
			return fmt.Errorf("preflight Taproot Asset confirmed " +
				"proof: anchor inventory is incomplete")
		}
		if verification.PassiveAssetCount != 0 {
			return fmt.Errorf("preflight Taproot Asset confirmed "+
				"proof: passive asset count is %d",
				verification.PassiveAssetCount)
		}

		return nil
	}

	if _, err := s.proofPath.Clone().Verify(ctx, s.verifier); err != nil {
		return fmt.Errorf("preflight Taproot Asset proof path: %w", err)
	}

	return nil
}

type assetRecipientSpec struct {
	logicalID      string
	recipientIndex int
	amount         uint64
	proofDelivery  tapsdk.CustomAssetProofDelivery
}

type arkBuild struct {
	request  *tapsdk.CustomAnchorRequest
	verifier tapsdk.ConfirmedProofVerifier
}

// plannedRecipients derives every local change output once and persists the
// uncomposed policies before any external commit can occur.
func (p *Preparer) plannedRecipients(ctx context.Context,
	request *oor.TaprootAssetOORPrepareRequest, state *preparationState) (
	[]oortx.RecipientOutput, error) {

	if len(state.PlannedRecipients) != 0 {
		if err := validatePlannedRecipients(
			request, state.PlannedRecipients,
		); err != nil {
			return nil, fmt.Errorf("restore Taproot Asset "+
				"recipients: %w", err)
		}

		return cloneRecipients(state.PlannedRecipients), nil
	}

	recipients := cloneRecipients(request.Recipients)
	plan, err := request.CarrierAllocation()
	if err != nil {
		return nil, err
	}
	if plan.AssetChange > 0 {
		change, err := request.BuildChangeRecipient(
			ctx, plan.AssetChange,
		)
		if err != nil {
			return nil, fmt.Errorf("derive Taproot Asset "+
				"change: %w", err)
		}
		if err := validateUncomposedChange(
			change, plan.AssetChange,
		); err != nil {
			return nil, fmt.Errorf("Taproot Asset change: %w", err)
		}
		recipients = append(recipients, change)
	}

	// The sender's carrier returns only when the spent leaf was
	// round-created; a reclaim-only send derives no wallet change at all.
	if plan.SenderChange > 0 {
		change, err := request.BuildChangeRecipient(
			ctx, plan.SenderChange,
		)
		if err != nil {
			return nil, fmt.Errorf("derive Bitcoin change: %w", err)
		}
		if err := validateUncomposedChange(
			change, plan.SenderChange,
		); err != nil {
			return nil, fmt.Errorf("Bitcoin change: %w", err)
		}
		recipients = append(recipients, change)
	}

	// The float residual returns to the lease pkScript verbatim: a plain
	// recipient output under the float's own policy, so BIP-371 output
	// metadata and VTXO registration derive from the lease template.
	if plan.OperatorChange > 0 {
		recipients = append(recipients, oortx.RecipientOutput{
			PkScript: append(
				[]byte(nil), request.Lease.PkScript...,
			),
			Value: plan.OperatorChange,
			VTXOPolicyTemplate: append(
				[]byte(nil), request.Lease.PolicyTemplate...,
			),
		})
	}
	if err := validatePlannedRecipients(request, recipients); err != nil {
		return nil, err
	}

	state.PlannedRecipients = cloneRecipients(recipients)
	if err := p.storeState(ctx, request.RequestID, state); err != nil {
		return nil, fmt.Errorf("persist Taproot Asset recipients: %w",
			err)
	}

	return recipients, nil
}

func validatePlannedRecipients(request *oor.TaprootAssetOORPrepareRequest,
	recipients []oortx.RecipientOutput) error {

	if request == nil || len(request.Recipients) != 1 {
		return fmt.Errorf("caller receiver is required")
	}
	plan, err := request.CarrierAllocation()
	if err != nil {
		return err
	}
	expectedCount := 1
	if plan.AssetChange != 0 {
		expectedCount++
	}
	if plan.SenderChange != 0 {
		expectedCount++
	}
	if plan.OperatorChange != 0 {
		expectedCount++
	}
	if len(recipients) != expectedCount {
		return fmt.Errorf("recipient count is %d, want %d",
			len(recipients), expectedCount)
	}

	receiver := recipients[0]
	wantReceiver := request.Recipients[0]
	if receiver.Value != wantReceiver.Value ||
		!bytes.Equal(receiver.PkScript, wantReceiver.PkScript) ||
		!bytes.Equal(
			receiver.VTXOPolicyTemplate,
			wantReceiver.VTXOPolicyTemplate,
		) {
		return fmt.Errorf("caller receiver changed")
	}
	if err := validateUncomposedRecipient(receiver); err != nil {
		return fmt.Errorf("caller receiver: %w", err)
	}

	next := 1
	if plan.AssetChange != 0 {
		if err := validateUncomposedChange(
			recipients[next], plan.AssetChange,
		); err != nil {
			return fmt.Errorf("Taproot Asset change: %w", err)
		}
		next++
	}
	if plan.SenderChange != 0 {
		if err := validateUncomposedChange(
			recipients[next], plan.SenderChange,
		); err != nil {
			return fmt.Errorf("Bitcoin change: %w", err)
		}
		next++
	}
	if plan.OperatorChange != 0 {
		operatorChange := recipients[next]
		if operatorChange.Value != plan.OperatorChange ||
			!bytes.Equal(
				operatorChange.PkScript, request.Lease.PkScript,
			) ||
			!bytes.Equal(
				operatorChange.VTXOPolicyTemplate,
				request.Lease.PolicyTemplate,
			) {
			return fmt.Errorf("operator change does not pay the " +
				"leased float script")
		}
		err := validateUncomposedRecipient(operatorChange)
		if err != nil {
			return fmt.Errorf("operator change: %w", err)
		}
	}

	seenOutputs := make(map[string]struct{}, len(recipients))
	for idx := range recipients {
		recipient := recipients[idx]
		_, policy, err := recipientAnchorPlan(recipient)
		if err != nil {
			return fmt.Errorf("recipient %d policy: %w", idx, err)
		}
		expectedPkScript, err := txscript.PayToTaprootScript(
			policy.OutputKey(),
		)
		if err != nil {
			return fmt.Errorf("recipient %d policy script: %w", idx,
				err)
		}
		if !bytes.Equal(recipient.PkScript, expectedPkScript) {
			return fmt.Errorf("recipient %d pkScript does not "+
				"match its policy", idx)
		}

		identity := fmt.Sprintf("%d:%x", recipient.Value,
			recipient.PkScript)
		if _, ok := seenOutputs[identity]; ok {
			return fmt.Errorf("recipient %d duplicates an earlier "+
				"value and pkScript", idx)
		}
		seenOutputs[identity] = struct{}{}
	}

	return nil
}

func validateUncomposedRecipient(recipient oortx.RecipientOutput) error {
	if recipient.TaprootAssetRoot != nil ||
		recipient.TaprootAssetRef != "" ||
		recipient.TaprootAssetAmount != 0 {
		return fmt.Errorf("asset metadata must be empty before " +
			"composition")
	}

	return nil
}

func validateUncomposedChange(recipient oortx.RecipientOutput,
	want btcutil.Amount) error {

	if recipient.Value != want {
		return fmt.Errorf("carrier value is %d, want %d",
			recipient.Value, want)
	}
	if len(recipient.PkScript) == 0 ||
		len(recipient.VTXOPolicyTemplate) == 0 {
		return fmt.Errorf("policy and pkScript are required")
	}
	if err := validateUncomposedRecipient(recipient); err != nil {
		return fmt.Errorf("change builder returned asset metadata: %w",
			err)
	}

	return nil
}

// resolveAssetSpendSource chooses either the initial managed proof or the
// exact sealed package lineage that created a chained OP_TRUE VTXO.
func (p *Preparer) resolveAssetSpendSource(ctx context.Context,
	request *oor.TaprootAssetOORPrepareRequest, assetInputIndex int,
	assetRef tapsdk.AssetRef) (*assetSpendSource, error) {

	input := &request.Inputs[assetInputIndex]
	if len(request.Intent.ProofFile) != 0 {
		return &assetSpendSource{
			proofFile: append(
				[]byte(nil), request.Intent.ProofFile...,
			),
			witness: tapsdk.CustomAssetWitnessPlan{
				Mode: witnessBackendSigner,
			},
			verifier: &proofInventoryVerifier{
				client:    p.inventory,
				assetRef:  assetRef,
				amount:    request.Intent.AssetAmount,
				anchor:    sdkOutpoint(input.VTXO.Outpoint),
				assetRoot: tapsdk.Hash(*input.TaprootAssetRoot),
			},
		}, nil
	}
	if p.loadCreatedPackage == nil {
		return nil, fmt.Errorf("Taproot Asset input proof or exact " +
			"created package is required")
	}

	packageBytes, err := p.loadCreatedPackage(
		ctx, input.VTXO.Outpoint,
	)
	if err != nil {
		return nil, fmt.Errorf("load created Taproot Asset package: %w",
			err)
	}
	resolved, err := ResolveCreatedAssetProofSource(
		packageBytes, input.VTXO.Outpoint, int64(input.VTXO.Amount),
		input.VTXO.TaprootAssetRef, input.VTXO.TaprootAssetAmount,
		*input.TaprootAssetRoot,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve created Taproot Asset "+
			"proof: %w", err)
	}
	var path tapsdk.AssetProofPath
	if err := path.UnmarshalBinary(resolved.CompactProofPath); err != nil {
		return nil, fmt.Errorf("decode created Taproot Asset path: %w",
			err)
	}
	lineageClient, ok := p.inventory.(proofLineageClient)
	if !ok {
		return nil, fmt.Errorf("tapd proof lineage client is required")
	}

	return &assetSpendSource{
		proofPath: path.Clone(),
		witness: tapsdk.CustomAssetWitnessPlan{
			Mode:  witnessCallerProvided,
			Stack: cloneByteSlices(resolved.OPTrueWitness),
		},
		verifier: &proofLineageVerifier{
			client: lineageClient,
		},
	}, nil
}

func (s *assetSpendSource) customInput(id string, assetRef tapsdk.AssetRef,
	amount uint64) (tapsdk.CustomAssetInput, error) {

	if s == nil {
		return tapsdk.CustomAssetInput{}, fmt.Errorf("Taproot Asset " +
			"proof source is required")
	}
	input := tapsdk.CustomAssetInput{
		ID:       id,
		AssetRef: assetRef,
		Amount:   amount,
		Witness: tapsdk.CustomAssetWitnessPlan{
			Mode:  s.witness.Mode,
			Stack: cloneByteSlices(s.witness.Stack),
		},
	}
	switch {
	case len(s.proofFile) != 0:
		input.ProofFile = append([]byte(nil), s.proofFile...)

	case s.proofPath != nil:
		input.ProofPath = s.proofPath.Clone()

	default:
		return tapsdk.CustomAssetInput{}, fmt.Errorf("Taproot Asset " +
			"proof source is empty")
	}

	return input, nil
}

func (s *assetSpendSource) appendTransition(proofBlob []byte, witness [][]byte,
	expected *expectedUnconfirmedAnchor) (tapsdk.CustomAssetInput,
	tapsdk.ConfirmedProofVerifier, error) {

	if s == nil || len(proofBlob) == 0 {
		return tapsdk.CustomAssetInput{}, nil, fmt.Errorf(
			"checkpoint transition proof is required")
	}
	var path *tapsdk.AssetProofPath
	if s.proofPath != nil {
		path = s.proofPath.Clone()
	} else {
		path = &tapsdk.AssetProofPath{
			Version: tapsdk.AssetProofPathVersionV0,
			ConfirmedBaseProof: append(
				[]byte(nil), s.proofFile...,
			),
		}
	}
	if len(path.Steps) >= tapsdk.AssetProofPathMaxDepth {
		return tapsdk.CustomAssetInput{}, nil, fmt.Errorf("Taproot " +
			"Asset proof path has no remaining transition slot")
	}
	path.Steps = append(path.Steps, tapsdk.AssetProofPathStep{
		TransitionProof: append([]byte(nil), proofBlob...),
	})
	expected.stepIndex = uint16(len(path.Steps) - 1)

	verifier := verifierWithExpectedLast(s.verifier, expected)

	return tapsdk.CustomAssetInput{
		ProofPath: path,
		Witness: tapsdk.CustomAssetWitnessPlan{
			Mode:  witnessCallerProvided,
			Stack: cloneByteSlices(witness),
		},
	}, verifier, nil
}

func verifierWithExpectedLast(verifier tapsdk.ConfirmedProofVerifier,
	expected *expectedUnconfirmedAnchor) tapsdk.ConfirmedProofVerifier {

	switch typed := verifier.(type) {
	case *proofInventoryVerifier:
		clone := *typed
		clone.unconfirmed = expected

		return &clone

	case *proofLineageVerifier:
		clone := *typed
		clone.expectedLast = expected

		return &clone

	default:
		return verifier
	}
}

// prepareCheckpoints builds every Bitcoin checkpoint and commits only the
// unique asset-bearing checkpoint through tap-sdk.
func (p *Preparer) prepareCheckpoints(ctx context.Context,
	request *oor.TaprootAssetOORPrepareRequest, assetInputIndex int,
	assetRef tapsdk.AssetRef, digest tapsdk.Hash, source *assetSpendSource,
	state *preparationState) ([]*psbt.Packet, []*oortx.CheckpointArtifact,
	*commitResult, error) {

	checkpoints := make([]*psbt.Packet, len(request.Inputs))
	artifacts := make([]*oortx.CheckpointArtifact, len(request.Inputs))
	for idx := range request.Inputs {
		input := &request.Inputs[idx]
		checkpointInput, err := input.CheckpointInput()
		if err != nil {
			return nil, nil, nil, err
		}
		artifact, err := oortx.BuildCheckpointPSBT(
			request.Policy, checkpointInput,
		)
		if err != nil {
			return nil, nil, nil, err
		}
		spendPath, err := input.EffectiveSpendPath()
		if err != nil {
			return nil, nil, nil, err
		}
		if err := psbtutil.AddTapLeafScript(
			&artifact.PSBT.Inputs[0], spendPath.SpendInfo,
		); err != nil {
			return nil, nil, nil, err
		}
		artifacts[idx] = artifact
		checkpoints[idx] = artifact.PSBT
	}

	committed, err := p.commitAssetCheckpoint(
		ctx, request, assetInputIndex, assetRef, digest, source,
		artifacts[assetInputIndex], state,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	checkpoint, err := psbtutil.Parse(committed.anchorPSBT)
	if err != nil {
		return nil, nil, nil, err
	}
	checkpoints[assetInputIndex] = checkpoint
	if err := validateCheckpointResult(
		request, assetInputIndex, assetRef, checkpoint, committed,
	); err != nil {
		return nil, nil, nil, err
	}

	return checkpoints, artifacts, committed, nil
}

func (p *Preparer) commitAssetCheckpoint(ctx context.Context,
	request *oor.TaprootAssetOORPrepareRequest, assetInputIndex int,
	assetRef tapsdk.AssetRef, digest tapsdk.Hash, source *assetSpendSource,
	artifact *oortx.CheckpointArtifact, state *preparationState) (
	*commitResult, error) {

	if len(state.CheckpointPackage) != 0 {
		committed, err := p.driver.DecodePackage(
			state.CheckpointPackage,
		)
		if err != nil {
			return nil, fmt.Errorf("restore checkpoint package: %w",
				err)
		}

		return committed, nil
	}
	input := &request.Inputs[assetInputIndex]
	assetInput, err := source.customInput(
		"wavelength-input", assetRef, request.Intent.AssetAmount,
	)
	if err != nil {
		return nil, err
	}
	anchorPlan, err := checkpointAnchorPlan(
		request.Policy, input.OwnerLeafScript,
	)
	if err != nil {
		return nil, err
	}
	anchorBytes, err := psbtutil.Serialize(artifact.PSBT)
	if err != nil {
		return nil, err
	}
	spendPath, err := input.EffectiveSpendPath()
	if err != nil {
		return nil, err
	}
	opTrueKey := deterministicKey(digest, attemptCheckpoint)
	sdkRequest := &tapsdk.CustomAnchorRequest{
		Inputs: []tapsdk.CustomAssetInput{
			assetInput,
		},
		Outputs: []tapsdk.CustomAssetOutput{{
			ID:                "wavelength-checkpoint",
			AssetRef:          assetRef,
			Amount:            request.Intent.AssetAmount,
			AnchorOutputIndex: 0,
			AnchorValueSat:    uint64(input.VTXO.Amount),
			Script: tapsdk.CustomAssetScriptPlan{
				Mode: tapsdk.CustomAssetScriptOPTrue,
				OPTrue: &tapsdk.CustomAssetOPTrueScriptPlan{
					InternalKey: tapsdk.KeyDescriptor{
						RawKeyBytes: opTrueKey,
					},
				},
			},
			Anchor: anchorPlan,
		}},
		AnchorPSBT: anchorBytes,
		Funding:    callerFundedExact(),
		PassiveAssets: tapsdk.CustomAnchorPassiveAssets{
			Policy: tapsdk.CustomAnchorPassiveReject,
		},
		LossPolicy: tapsdk.CustomAnchorLossPolicy{
			Mode: tapsdk.CustomAnchorLossReject,
		},
		SigningPlans: []tapsdk.CustomAnchorInputSigningPlan{
			scriptSigningPlan(
				0, spendPath.WitnessScript,
				input.VTXO.ClientKey.PubKey,
				input.VTXO.OperatorKey,
			),
		},
	}

	committed, err := p.commit(
		ctx, request.RequestID, state, attemptCheckpoint, sdkRequest,
		source.verifier,
	)
	if err != nil {
		return nil, err
	}
	state.CheckpointPackage = append(
		[]byte(nil), committed.packageBytes...,
	)
	state.Attempt = ""
	if err := p.storeState(ctx, request.RequestID, state); err != nil {
		state.Attempt = attemptCheckpoint

		return nil, fmt.Errorf("persist checkpoint package: %w", err)
	}

	return committed, nil
}

// prepareMixedArk restores or commits the checkpoint-to-recipient transition.
func (p *Preparer) prepareMixedArk(ctx context.Context,
	request *oor.TaprootAssetOORPrepareRequest, assetInputIndex int,
	assetRef tapsdk.AssetRef, checkpoints []*psbt.Packet,
	artifacts []*oortx.CheckpointArtifact, checkpointResult *commitResult,
	planned []oortx.RecipientOutput, digest tapsdk.Hash,
	source *assetSpendSource, state *preparationState) (*psbt.Packet,
	*commitResult, []oortx.RecipientOutput, error) {

	checkpointOutputs, err := checkpointOutputs(checkpoints, artifacts)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(state.ArkPackage) != 0 {
		committed, err := p.driver.DecodePackage(state.ArkPackage)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("restore Ark "+
				"package: %w", err)
		}
		ark, err := psbtutil.Parse(committed.anchorPSBT)
		if err != nil {
			return nil, nil, nil, err
		}
		recipients, err := validateMixedArkResult(
			request, assetInputIndex, assetRef, ark,
			checkpointOutputs, checkpointResult, planned, committed,
			nil,
		)

		return ark, committed, recipients, err
	}

	build, previews, _, nonce, err := p.stableArkBuild(
		ctx, request, assetInputIndex, assetRef, checkpointOutputs,
		artifacts, checkpointResult, planned, digest, source,
		serializeTx(checkpoints[assetInputIndex].UnsignedTx),
		state.OrderingNonce,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	if state.OrderingNonce != nonce {
		state.OrderingNonce = nonce
		if err := p.storeState(
			ctx, request.RequestID, state,
		); err != nil {
			return nil, nil, nil, fmt.Errorf("persist Taproot "+
				"Asset output ordering: %w", err)
		}
	}

	committed, err := p.commit(
		ctx, request.RequestID, state, attemptArk, build.request,
		build.verifier,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	state.ArkPackage = append([]byte(nil), committed.packageBytes...)
	state.Attempt = ""
	if err := p.storeState(ctx, request.RequestID, state); err != nil {
		state.Attempt = attemptArk

		return nil, nil, nil, fmt.Errorf("persist Ark package: %w", err)
	}
	ark, err := psbtutil.Parse(committed.anchorPSBT)
	if err != nil {
		return nil, nil, nil, err
	}
	recipients, err := validateMixedArkResult(
		request, assetInputIndex, assetRef, ark, checkpointOutputs,
		checkpointResult, planned, committed, previews,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	return ark, committed, recipients, nil
}

func checkpointOutputs(checkpoints []*psbt.Packet,
	artifacts []*oortx.CheckpointArtifact) ([]oortx.CheckpointOutput,
	error) {

	if len(checkpoints) != len(artifacts) {
		return nil, fmt.Errorf("checkpoint artifact count mismatch")
	}
	result := make([]oortx.CheckpointOutput, len(checkpoints))
	for idx := range checkpoints {
		checkpoint := checkpoints[idx]
		if checkpoint == nil || checkpoint.UnsignedTx == nil ||
			len(checkpoint.UnsignedTx.TxOut) == 0 ||
			artifacts[idx] == nil {
			return nil, fmt.Errorf("checkpoint %d is incomplete",
				idx)
		}
		result[idx] = oortx.CheckpointOutput{
			Txid:            checkpoint.UnsignedTx.TxHash(),
			Output:          checkpoint.UnsignedTx.TxOut[0],
			TapTreeEncoded:  artifacts[idx].TapTreeEncoded,
			OwnerLeafScript: artifacts[idx].OwnerLeafScript,
			OwnerLeafPolicy: artifacts[idx].OwnerLeafPolicy,
		}
	}

	return result, nil
}

func (p *Preparer) stableArkBuild(ctx context.Context,
	request *oor.TaprootAssetOORPrepareRequest, assetInputIndex int,
	assetRef tapsdk.AssetRef, checkpoints []oortx.CheckpointOutput,
	artifacts []*oortx.CheckpointArtifact, checkpointResult *commitResult,
	planned []oortx.RecipientOutput, digest tapsdk.Hash,
	source *assetSpendSource, assetCheckpointTx []byte, firstNonce uint32) (
	*arkBuild, []commitmentPreview, []oortx.RecipientOutput, uint32,
	error) {

	if firstNonce >= maxOrderingNonces {
		return nil, nil, nil, 0, fmt.Errorf("Taproot Asset output " +
			"ordering nonce is out of range")
	}
	for nonce := firstNonce; nonce < maxOrderingNonces; nonce++ {
		current := cloneRecipients(planned)
		seen := make(map[string]struct{})
		for iteration := 0; iteration <
			maxOrderingIterations; iteration++ {

			build, err := buildArkRequest(
				request, assetInputIndex, assetRef, checkpoints,
				artifacts, checkpointResult, planned, current,
				digest, source, assetCheckpointTx, nonce,
			)
			if err != nil {
				return nil, nil, nil, 0, err
			}
			previews, err := p.driver.Preview(
				ctx, build.request, build.verifier,
			)
			if err != nil {
				return nil, nil, nil, 0, err
			}
			composed, err := applyArkPreviews(
				request, planned, build.request.Outputs,
				previews,
			)
			if err != nil {
				return nil, nil, nil, 0, err
			}
			stable, vector, err := previewIndicesStable(
				composed, previews,
			)
			if err != nil {
				return nil, nil, nil, 0, err
			}
			if stable {
				finalBuild, err := buildArkRequest(
					request, assetInputIndex, assetRef,
					checkpoints, artifacts,
					checkpointResult, planned, composed,
					digest, source, assetCheckpointTx,
					nonce,
				)
				if err != nil {
					return nil, nil, nil, 0, err
				}
				finalPreview, err := p.driver.Preview(
					ctx, finalBuild.request,
					finalBuild.verifier,
				)
				if err != nil {
					return nil, nil, nil, 0, err
				}
				finalRecipients, err := applyArkPreviews(
					request, planned,
					finalBuild.request.Outputs,
					finalPreview,
				)
				if err != nil {
					return nil, nil, nil, 0, err
				}
				finalStable, _, err := previewIndicesStable(
					finalRecipients, finalPreview,
				)
				if err != nil {
					return nil, nil, nil, 0, err
				}
				if finalStable && recipientsEqual(
					composed, finalRecipients,
				) {
					return finalBuild, finalPreview,
						finalRecipients, nonce, nil
				}
			}

			if _, ok := seen[vector]; ok {
				break
			}
			seen[vector] = struct{}{}
			current = composed
		}
	}

	return nil, nil, nil, 0, fmt.Errorf("Taproot Asset canonical output " +
		"ordering did not converge")
}

func buildArkRequest(request *oor.TaprootAssetOORPrepareRequest,
	assetInputIndex int, assetRef tapsdk.AssetRef,
	checkpoints []oortx.CheckpointOutput,
	artifacts []*oortx.CheckpointArtifact, checkpointResult *commitResult,
	planned, current []oortx.RecipientOutput, digest tapsdk.Hash,
	source *assetSpendSource, assetCheckpointTx []byte, nonce uint32) (
	*arkBuild, error) {

	ark, err := oortx.BuildArkPSBT(checkpoints, current)
	if err != nil {
		return nil, err
	}
	inputPositions := make(
		map[wire.OutPoint]uint32, len(ark.UnsignedTx.TxIn),
	)
	for idx := range ark.UnsignedTx.TxIn {
		inputPositions[ark.UnsignedTx.TxIn[idx].PreviousOutPoint] =
			uint32(idx)
	}
	assetCheckpoint := checkpointResult.outputs[0]
	signingPlans := make(
		[]tapsdk.CustomAnchorInputSigningPlan, len(checkpoints),
	)
	for idx := range checkpoints {
		outpoint := wire.OutPoint{
			Hash: checkpoints[idx].Txid, Index: 0,
		}
		arkIndex, ok := inputPositions[outpoint]
		if !ok {
			return nil, fmt.Errorf("checkpoint %d is absent from "+
				"Ark inputs", idx)
		}
		leaf, err := oortx.BuildTaprootTapLeafScript(
			artifacts[idx].TapTreeEncoded,
			artifacts[idx].OwnerLeafScript,
		)
		if err != nil {
			return nil, err
		}
		if idx == assetInputIndex {
			leaf, err = composeTapLeaf(
				leaf, assetCheckpoint.taprootAssetRoot,
			)
			if err != nil {
				return nil, err
			}
		}
		ark.Inputs[arkIndex].TaprootLeafScript =
			[]*psbt.TaprootTapLeafScript{leaf}
		signers, err := ownerLeafSigners(&request.Inputs[idx])
		if err != nil {
			return nil, fmt.Errorf("input %d signers: %w", idx, err)
		}
		signingPlans[idx] = scriptSigningPlan(
			arkIndex, artifacts[idx].OwnerLeafScript, signers...,
		)
	}

	transitionInput, verifier, err := source.appendTransition(
		assetCheckpoint.proofBlob, assetCheckpoint.opTrueWitness,
		&expectedUnconfirmedAnchor{
			previousOutpoint: sdkOutpoint(
				request.Inputs[assetInputIndex].VTXO.Outpoint,
			),
			anchorOutpoint: assetCheckpoint.anchorOutpoint,
			transaction: append(
				[]byte(nil), assetCheckpointTx...,
			),
		},
	)
	if err != nil {
		return nil, err
	}
	transitionInput.ID = "wavelength-checkpoint"
	transitionInput.AssetRef = assetRef
	transitionInput.Amount = request.Intent.AssetAmount

	specs := assetRecipientSpecs(request)
	outputs := make([]tapsdk.CustomAssetOutput, len(specs))
	assetOutputIndices := make(map[uint32]struct{}, len(specs))
	for idx := range specs {
		spec := specs[idx]
		recipient := planned[spec.recipientIndex]
		anchorPlan, _, err := recipientAnchorPlan(recipient)
		if err != nil {
			return nil, err
		}
		outputIndex, err := oortx.RecipientOutputIndex(
			current, current[spec.recipientIndex],
		)
		if err != nil {
			return nil, err
		}
		assetOutputIndices[outputIndex] = struct{}{}
		opTrueKey := deterministicKey(
			digest, fmt.Sprintf("ark/%d/%s", nonce, spec.logicalID),
		)
		outputs[idx] = tapsdk.CustomAssetOutput{
			ID:                spec.logicalID,
			AssetRef:          assetRef,
			Amount:            spec.amount,
			AnchorOutputIndex: outputIndex,
			AnchorValueSat:    uint64(recipient.Value),
			Script: tapsdk.CustomAssetScriptPlan{
				Mode: tapsdk.CustomAssetScriptOPTrue,
				OPTrue: &tapsdk.CustomAssetOPTrueScriptPlan{
					InternalKey: tapsdk.KeyDescriptor{
						RawKeyBytes: opTrueKey,
					},
				},
			},
			Anchor:        anchorPlan,
			ProofDelivery: spec.proofDelivery,
		}
	}
	if err := addNonAssetArkOutputMetadata(
		ark, current, assetOutputIndices,
	); err != nil {
		return nil, err
	}
	anchorBytes, err := psbtutil.Serialize(ark)
	if err != nil {
		return nil, err
	}

	return &arkBuild{
		request: &tapsdk.CustomAnchorRequest{
			Inputs: []tapsdk.CustomAssetInput{
				transitionInput,
			},
			Outputs:    outputs,
			AnchorPSBT: anchorBytes,
			Funding:    callerFundedExact(),
			PassiveAssets: tapsdk.CustomAnchorPassiveAssets{
				Policy: tapsdk.CustomAnchorPassiveReject,
			},
			LossPolicy: tapsdk.CustomAnchorLossPolicy{
				Mode: tapsdk.CustomAnchorLossReject,
			},
			SigningPlans: signingPlans,
		},
		verifier: verifier,
	}, nil
}

// ownerLeafSigners returns the keys that must sign an input's checkpoint
// owner leaf in the Ark transaction. An operator-funded float input has no
// local client key; its signers are the lease policy's own collab pair.
func ownerLeafSigners(input *oor.TransferInput) ([]*btcec.PublicKey, error) {
	if !input.OperatorFunded {
		return []*btcec.PublicKey{
			input.VTXO.ClientKey.PubKey,
			input.VTXO.OperatorKey,
		}, nil
	}

	leaf, err := arkscript.DecodeLeafTemplate(input.OwnerLeafPolicy)
	if err != nil {
		return nil, fmt.Errorf("decode float owner leaf: %w", err)
	}

	multisig, ok := leaf.Node.(*arkscript.Multisig)
	if !ok || len(multisig.Keys) == 0 {
		return nil, fmt.Errorf("float owner leaf is not a multisig")
	}

	return append([]*btcec.PublicKey(nil), multisig.Keys...), nil
}

// addNonAssetArkOutputMetadata attaches the complete standard PSBT Taproot
// output metadata for Bitcoin-only VTXOs. Asset-bearing outputs are described
// by their CustomAssetOutput plans and are completed by tap-sdk.
func addNonAssetArkOutputMetadata(ark *psbt.Packet,
	recipients []oortx.RecipientOutput,
	assetOutputIndices map[uint32]struct{}) error {

	if ark == nil || ark.UnsignedTx == nil {
		return fmt.Errorf("Ark PSBT is required")
	}
	canonical := oortx.CanonicalRecipientOutputs(recipients)
	if len(ark.Outputs) != len(canonical)+1 {
		return fmt.Errorf("Ark PSBT output count mismatch")
	}

	for idx := range canonical {
		outputIndex := uint32(idx)
		if _, ok := assetOutputIndices[outputIndex]; ok {
			continue
		}

		_, policy, err := recipientAnchorPlan(canonical[idx])
		if err != nil {
			return fmt.Errorf("Bitcoin-only recipient %d "+
				"policy: %w", idx, err)
		}
		tapTree, err := encodeBIP371TapTree(policy)
		if err != nil {
			return fmt.Errorf("Bitcoin-only recipient %d tap "+
				"tree: %w", idx, err)
		}

		ark.Outputs[idx].TaprootInternalKey = schnorr.SerializePubKey(
			policy.InternalKey,
		)
		ark.Outputs[idx].TaprootTapTree = tapTree
	}

	return nil
}

// encodeBIP371TapTree serializes an Ark policy into PSBT_OUT_TAP_TREE. This is
// deliberately distinct from arkscript.EncodeTapTree, whose leading leaf count
// is part of Wavelength's private checkpoint encoding.
func encodeBIP371TapTree(policy *arkscript.CompiledPolicy) ([]byte, error) {
	if policy == nil || policy.InternalKey == nil ||
		len(policy.Leaves) == 0 {
		return nil, fmt.Errorf("compiled policy is required")
	}

	var encoded bytes.Buffer
	for idx := range policy.Leaves {
		spendInfo, err := policy.SpendInfo(idx)
		if err != nil {
			return nil, err
		}
		if len(spendInfo.ControlBlock) < 33 ||
			(len(spendInfo.ControlBlock)-33)%
				chainhash.HashSize != 0 {
			return nil, fmt.Errorf("leaf %d control block "+
				"is invalid", idx)
		}
		depth := (len(spendInfo.ControlBlock) - 33) /
			chainhash.HashSize
		if depth > 255 {
			return nil, fmt.Errorf("leaf %d depth exceeds 255", idx)
		}

		leaf := policy.Leaves[idx].Leaf
		if err := encoded.WriteByte(byte(depth)); err != nil {
			return nil, err
		}
		if err := encoded.WriteByte(
			byte(leaf.LeafVersion),
		); err != nil {
			return nil, err
		}
		if err := wire.WriteVarInt(
			&encoded, 0,
			uint64(
				len(leaf.Script),
			),
		); err != nil {
			return nil, err
		}
		if _, err := encoded.Write(leaf.Script); err != nil {
			return nil, err
		}
	}

	return encoded.Bytes(), nil
}

func assetRecipientSpecs(
	request *oor.TaprootAssetOORPrepareRequest) []assetRecipientSpec {

	receiverAmount := request.Intent.EffectiveRecipientAssetAmount()
	specs := []assetRecipientSpec{{
		logicalID:      receiverOutputID,
		recipientIndex: 0,
		amount:         receiverAmount,
		proofDelivery: tapsdk.CustomAssetProofDelivery{
			RecipientID:    request.RequestID,
			CourierAddress: request.Intent.ProofCourierAddress,
			OpaqueMetadata: append(
				[]byte(nil),
				request.Intent.ProofDeliveryMetadata...,
			),
		},
	}}
	if receiverAmount < request.Intent.AssetAmount {
		specs = append(specs, assetRecipientSpec{
			logicalID:      changeOutputID,
			recipientIndex: 1,
			amount: request.Intent.AssetAmount -
				receiverAmount,
			proofDelivery: tapsdk.CustomAssetProofDelivery{
				RecipientID: request.RequestID + "/change",
			},
		})
	}

	return specs
}

func applyArkPreviews(request *oor.TaprootAssetOORPrepareRequest,
	planned []oortx.RecipientOutput, outputs []tapsdk.CustomAssetOutput,
	previews []commitmentPreview) ([]oortx.RecipientOutput, error) {

	if len(outputs) != len(previews) {
		return nil, fmt.Errorf("Taproot Asset preview count mismatch")
	}
	byID := make(map[string]commitmentPreview, len(previews))
	for idx := range previews {
		preview := previews[idx]
		if _, ok := byID[preview.logicalOutputID]; ok {
			return nil, fmt.Errorf("duplicate Taproot Asset "+
				"preview %q", preview.logicalOutputID)
		}
		byID[preview.logicalOutputID] = preview
	}

	result := cloneRecipients(planned)
	for _, spec := range assetRecipientSpecs(request) {
		preview, ok := byID[spec.logicalID]
		if !ok {
			return nil, fmt.Errorf("missing Taproot Asset "+
				"preview %q", spec.logicalID)
		}
		var output *tapsdk.CustomAssetOutput
		for idx := range outputs {
			if outputs[idx].ID == spec.logicalID {
				output = &outputs[idx]
				break
			}
		}
		if output == nil ||
			preview.anchorOutputIndex != output.AnchorOutputIndex {
			return nil, fmt.Errorf("Taproot Asset preview index " +
				"mismatch")
		}
		recipient := &result[spec.recipientIndex]
		_, policy, err := recipientAnchorPlan(*recipient)
		if err != nil {
			return nil, err
		}
		combined := tapBranchHash(
			policy.RootHash, preview.assetRoot[:],
		)
		if tapsdk.Hash(combined) != preview.merkleRoot {
			return nil, fmt.Errorf("Taproot Asset preview merkle " +
				"root mismatch")
		}
		root := chainhash.Hash(preview.assetRoot)
		composed, err := arkscript.ComposeWithSiblingRoot(policy, root)
		if err != nil {
			return nil, err
		}
		recipient.PkScript, err = txscript.PayToTaprootScript(
			composed.OutputKey(),
		)
		if err != nil {
			return nil, err
		}
		recipient.TaprootAssetRoot = &root
		recipient.TaprootAssetRef = request.Intent.AssetRef
		recipient.TaprootAssetAmount = spec.amount
	}

	return result, nil
}

func previewIndicesStable(recipients []oortx.RecipientOutput,
	previews []commitmentPreview) (bool, string, error) {

	stable := true
	vector := ""
	for idx := range previews {
		preview := previews[idx]
		var recipient *oortx.RecipientOutput
		for candidate := range recipients {
			logicalID := receiverOutputID
			if candidate == 1 && len(previews) > 1 {
				logicalID = changeOutputID
			}
			if logicalID == preview.logicalOutputID {
				recipient = &recipients[candidate]
				break
			}
		}
		if recipient == nil {
			return false, "", fmt.Errorf("unknown Taproot Asset "+
				"preview %q", preview.logicalOutputID)
		}
		outputIndex, err := oortx.RecipientOutputIndex(
			recipients, *recipient,
		)
		if err != nil {
			return false, "", err
		}
		if outputIndex != preview.anchorOutputIndex {
			stable = false
		}
		vector += fmt.Sprintf("%s:%d;", preview.logicalOutputID,
			outputIndex)
	}

	return stable, vector, nil
}

// validateMixedArkResult binds the sealed tap-sdk package to every Ark input
// and recipient while preserving Bitcoin-only change as an unrooted output.
func validateMixedArkResult(request *oor.TaprootAssetOORPrepareRequest,
	assetInputIndex int, assetRef tapsdk.AssetRef, ark *psbt.Packet,
	checkpoints []oortx.CheckpointOutput, checkpoint *commitResult,
	planned []oortx.RecipientOutput, result *commitResult,
	expectedPreviews []commitmentPreview) ([]oortx.RecipientOutput, error) {

	if ark == nil || ark.UnsignedTx == nil {
		return nil, fmt.Errorf("committed Ark PSBT is required")
	}
	specs := assetRecipientSpecs(request)
	if result == nil || len(result.inputs) != 1 ||
		len(result.outputs) != len(specs) {
		return nil, fmt.Errorf("Ark package asset cardinality mismatch")
	}
	if result.fundingMode != tapsdk.CustomAnchorFundingCallerFundedExact ||
		result.actualFeeSat != 0 || result.maxFeeSat != 0 {
		return nil, fmt.Errorf("Ark package funding mode mismatch")
	}

	assetCheckpoint := checkpoint.outputs[0]
	assetArkInput, err := findArkInputIndex(
		ark, wire.OutPoint{
			Hash:  checkpoints[assetInputIndex].Txid,
			Index: 0,
		},
	)
	if err != nil {
		return nil, err
	}
	input := result.inputs[0]
	if input.anchorOutpoint != assetCheckpoint.anchorOutpoint ||
		input.anchorInputIndex != assetArkInput ||
		!input.assetRef.Equivalent(assetRef) ||
		input.amount != request.Intent.AssetAmount {
		return nil, fmt.Errorf("Ark package asset input mismatch")
	}

	previewByID := make(map[string]commitmentPreview, len(expectedPreviews))
	for idx := range expectedPreviews {
		preview := expectedPreviews[idx]
		previewByID[preview.logicalOutputID] = preview
	}
	resultByID := make(map[string]commitOutput, len(result.outputs))
	for idx := range result.outputs {
		output := result.outputs[idx]
		if _, ok := resultByID[output.logicalOutputID]; ok {
			return nil, fmt.Errorf("duplicate Ark package "+
				"output %q", output.logicalOutputID)
		}
		resultByID[output.logicalOutputID] = output
	}

	recipients := cloneRecipients(planned)
	for _, spec := range specs {
		output, ok := resultByID[spec.logicalID]
		if !ok {
			return nil, fmt.Errorf("missing Ark package output %q",
				spec.logicalID)
		}
		if output.anchorOutputIndex >=
			uint32(len(ark.UnsignedTx.TxOut)) {
			return nil, fmt.Errorf("Ark package output index is " +
				"out of range")
		}
		txOut := ark.UnsignedTx.TxOut[output.anchorOutputIndex]
		if output.anchorOutpoint != sdkOutpoint(wire.OutPoint{
			Hash:  ark.UnsignedTx.TxHash(),
			Index: output.anchorOutputIndex,
		}) || !output.assetRef.Equivalent(assetRef) ||
			output.amount != spec.amount ||
			output.anchorValueSat !=
				int64(planned[spec.recipientIndex].Value) ||
			output.scriptMode != tapsdk.CustomAssetScriptOPTrue ||
			len(output.opTrueWitness) == 0 ||
			len(output.proofBlob) == 0 {
			return nil, fmt.Errorf("Ark package output %q mismatch",
				spec.logicalID)
		}
		if expected, ok := previewByID[spec.logicalID]; ok {
			if output.anchorOutputIndex !=
				expected.anchorOutputIndex ||
				output.taprootAssetRoot != expected.assetRoot ||
				output.taprootMerkleRoot !=
					expected.merkleRoot {
				return nil, fmt.Errorf("Ark commit diverged "+
					"from preview for %q", spec.logicalID)
			}
		}

		recipient := &recipients[spec.recipientIndex]
		root := chainhash.Hash(output.taprootAssetRoot)
		recipient.TaprootAssetRoot = &root
		recipient.TaprootAssetRef = output.assetRef.String()
		recipient.TaprootAssetAmount = output.amount
		_, policy, err := recipientAnchorPlan(*recipient)
		if err != nil {
			return nil, err
		}
		composed, err := arkscript.ComposeWithSiblingRoot(policy, root)
		if err != nil {
			return nil, err
		}
		recipient.PkScript, err = txscript.PayToTaprootScript(
			composed.OutputKey(),
		)
		if err != nil {
			return nil, err
		}
		if err := validateOutputCommitment(
			txOut, policy.InternalKey, policy.RootHash, output,
		); err != nil {
			return nil, fmt.Errorf("Ark output %q: %w",
				spec.logicalID, err)
		}
	}

	expectedArk, err := oortx.BuildArkPSBT(checkpoints, recipients)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(
		serializeTx(expectedArk.UnsignedTx),
		serializeTx(ark.UnsignedTx),
	) {
		return nil, fmt.Errorf("committed Ark transaction differs " +
			"from canonical recipients")
	}

	return recipients, nil
}

func findArkInputIndex(ark *psbt.Packet,
	outpoint wire.OutPoint) (uint32, error) {

	var (
		found bool
		index uint32
	)
	for idx := range ark.UnsignedTx.TxIn {
		if ark.UnsignedTx.TxIn[idx].PreviousOutPoint != outpoint {
			continue
		}
		if found {
			return 0, fmt.Errorf("Ark input outpoint is ambiguous")
		}
		found = true
		index = uint32(idx)
	}
	if !found {
		return 0, fmt.Errorf("Ark input outpoint is missing")
	}

	return index, nil
}

func recipientsEqual(left, right []oortx.RecipientOutput) bool {
	if len(left) != len(right) {
		return false
	}
	for idx := range left {
		if left[idx].Value != right[idx].Value ||
			!bytes.Equal(left[idx].PkScript, right[idx].PkScript) ||
			!bytes.Equal(
				left[idx].VTXOPolicyTemplate,
				right[idx].VTXOPolicyTemplate,
			) || left[idx].TaprootAssetRef !=
			right[idx].TaprootAssetRef ||
			left[idx].TaprootAssetAmount !=
				right[idx].TaprootAssetAmount ||
			!sameAssetRoot(
				left[idx].TaprootAssetRoot,
				right[idx].TaprootAssetRoot,
			) {
			return false
		}
	}

	return true
}

func sameAssetRoot(left, right *chainhash.Hash) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}

	return *left == *right
}
