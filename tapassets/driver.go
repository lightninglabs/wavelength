package tapassets

import (
	"context"
	"fmt"

	tapsdk "github.com/lightninglabs/tap-sdk"
)

type commitProofSource struct {
	kind      tapsdk.CustomAnchorProofSourceKind
	contentID tapsdk.Hash
	blob      []byte
}

type commitOutput struct {
	logicalOutputID    string
	logicalOutputIndex uint32
	packetIndex        uint32
	packetRole         tapsdk.CustomAnchorPacketRole
	virtualOutputIndex uint32
	anchorOutputIndex  uint32
	anchorOutpoint     tapsdk.Outpoint
	anchorValueSat     int64
	assetRef           tapsdk.AssetRef
	issuanceID         tapsdk.AssetID
	amount             uint64
	taprootAssetRoot   tapsdk.Hash
	taprootMerkleRoot  tapsdk.Hash
	scriptKey          tapsdk.PubKey
	scriptMode         tapsdk.CustomAssetScriptMode
	opTrueWitness      [][]byte
	proofBlob          []byte
}

type commitInput struct {
	logicalInputID    string
	logicalInputIndex uint32
	packetIndex       uint32
	packetRole        tapsdk.CustomAnchorPacketRole
	virtualInputIndex uint32
	anchorInputIndex  uint32
	anchorOutpoint    tapsdk.Outpoint
	assetRef          tapsdk.AssetRef
	issuanceID        tapsdk.AssetID
	scriptKey         tapsdk.PubKey
	amount            uint64
	proofSource       commitProofSource
}

type commitResult struct {
	packageBytes []byte
	anchorPSBT   []byte
	fundingMode  tapsdk.CustomAnchorFundingMode
	actualFeeSat uint64
	maxFeeSat    uint64
	inputs       []commitInput
	outputs      []commitOutput
}

type customAnchorDriver interface {
	Commit(context.Context, *tapsdk.CustomAnchorRequest,
		tapsdk.ConfirmedProofVerifier) (*commitResult, error)

	DecodePackage([]byte) (*commitResult, error)
}

type sdkDriver struct {
	wallet *tapsdk.Wallet
}

// Commit builds, commits, verifies, and seals one SDK custom-anchor request.
func (d *sdkDriver) Commit(ctx context.Context,
	request *tapsdk.CustomAnchorRequest,
	verifier tapsdk.ConfirmedProofVerifier) (*commitResult, error) {

	if d == nil || d.wallet == nil {
		return nil, fmt.Errorf("tap-sdk wallet is required")
	}

	builder := d.wallet.NewCustomAnchorTxBuilder()
	if verifier != nil {
		builder.SetConfirmedProofVerifier(verifier)
	}
	plan, err := builder.Build(ctx, request)
	if err != nil {
		return nil, err
	}

	result, err := plan.Commit(ctx, tapsdk.CustomAnchorCommitOptions{
		Publish: tapsdk.CustomAnchorPublishMetadata{
			SkipAnchorTxBroadcast: true,
			ExternalBroadcast:     true,
			Label:                 "wavelength-oor-poc",
		},
	})
	if err != nil {
		return nil, err
	}

	converted, err := commitResultFromPackage(result)
	if err != nil {
		return nil, &commitResponseError{err: err}
	}

	return converted, nil
}

// CommitOnboarding commits a custom anchor that tap-sdk itself will publish
// after Wavelength supplies the final Bitcoin signature.
func (d *sdkDriver) CommitOnboarding(ctx context.Context,
	request *tapsdk.CustomAnchorRequest,
	verifier tapsdk.ConfirmedProofVerifier) (*commitResult, error) {

	if d == nil || d.wallet == nil {
		return nil, fmt.Errorf("tap-sdk wallet is required")
	}

	builder := d.wallet.NewCustomAnchorTxBuilder()
	if verifier != nil {
		builder.SetConfirmedProofVerifier(verifier)
	}
	plan, err := builder.Build(ctx, request)
	if err != nil {
		return nil, err
	}

	result, err := plan.Commit(ctx, tapsdk.CustomAnchorCommitOptions{
		Publish: tapsdk.CustomAnchorPublishMetadata{
			Label: "wavelength-onboarding-poc",
		},
	})
	if err != nil {
		return nil, err
	}

	converted, err := commitResultFromPackage(result)
	if err != nil {
		return nil, &commitResponseError{err: err}
	}

	return converted, nil
}

// PublishOnboarding verifies the exact final PSBT and asks tap-sdk to publish
// and log the already committed asset transition.
func (d *sdkDriver) PublishOnboarding(ctx context.Context, packageBytes,
	finalPSBT []byte) error {

	if d == nil || d.wallet == nil {
		return fmt.Errorf("tap-sdk wallet is required")
	}

	var transfer tapsdk.CustomAnchorTransferPackage
	if err := transfer.UnmarshalBinary(packageBytes); err != nil {
		return fmt.Errorf("decode tap-sdk transfer package: %w", err)
	}
	if _, err := d.wallet.PublishCustomAnchorTransfer(
		ctx, &transfer, finalPSBT,
	); err != nil {
		return fmt.Errorf("publish tap-sdk onboarding transfer: %w",
			err)
	}

	return nil
}

// VerifyFinalOnboarding validates Wavelength's exact signed PSBT against the
// sealed tap-sdk package before either publishing or restoring it.
func (d *sdkDriver) VerifyFinalOnboarding(packageBytes,
	finalPSBT []byte) error {

	var transfer tapsdk.CustomAnchorTransferPackage
	if err := transfer.UnmarshalBinary(packageBytes); err != nil {
		return fmt.Errorf("decode tap-sdk transfer package: %w", err)
	}
	if err := transfer.VerifyFinalAnchorPSBT(finalPSBT); err != nil {
		return fmt.Errorf("verify final onboarding anchor PSBT: %w",
			err)
	}

	return nil
}

// DecodePackage restores a sealed SDK package from the durable journal.
func (d *sdkDriver) DecodePackage(encoded []byte) (*commitResult, error) {
	var transfer tapsdk.CustomAnchorTransferPackage
	if err := transfer.UnmarshalBinary(encoded); err != nil {
		return nil, fmt.Errorf("decode tap-sdk transfer package: %w",
			err)
	}

	return commitResultFromPackage(&transfer)
}

// commitResultFromPackage projects the SDK package into the narrow fields the
// Wavelength graph adapter needs while retaining the canonical package bytes.
func commitResultFromPackage(transfer *tapsdk.CustomAnchorTransferPackage) (
	*commitResult, error) {

	if transfer == nil {
		return nil, fmt.Errorf("tap-sdk transfer package is required")
	}
	if err := transfer.Validate(); err != nil {
		return nil, fmt.Errorf("validate tap-sdk transfer package: %w",
			err)
	}

	encoded, err := transfer.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("encode tap-sdk transfer package: %w",
			err)
	}
	result := &commitResult{
		packageBytes: encoded,
		anchorPSBT:   append([]byte(nil), transfer.AnchorPsbt...),
		fundingMode:  transfer.Funding.Mode,
		actualFeeSat: transfer.Funding.ActualFeeSat,
		maxFeeSat:    transfer.Funding.MaxFeeSat,
		inputs:       make([]commitInput, len(transfer.Inputs)),
		outputs:      make([]commitOutput, len(transfer.Outputs)),
	}
	for idx := range transfer.Inputs {
		input := transfer.Inputs[idx]
		result.inputs[idx] = commitInput{
			logicalInputID:    input.LogicalInputID,
			logicalInputIndex: input.LogicalInputIndex,
			packetIndex:       input.PacketIndex,
			packetRole:        input.PacketRole,
			virtualInputIndex: input.VirtualInputIndex,
			anchorInputIndex:  input.AnchorInputIndex,
			anchorOutpoint:    input.AnchorOutpoint,
			assetRef:          input.AssetRef,
			issuanceID:        input.IssuanceID,
			scriptKey:         input.ScriptKey,
			amount:            input.Amount,
			proofSource: commitProofSource{
				kind:      input.ProofSource.Kind,
				contentID: input.ProofSource.ContentID,
				blob: append(
					[]byte(nil), input.ProofSource.Blob...,
				),
			},
		}
	}
	for idx := range transfer.Outputs {
		output := transfer.Outputs[idx]
		var witness [][]byte
		if output.OPTrueSpend != nil {
			witness = output.OPTrueSpend.WitnessStack()
		}
		result.outputs[idx] = commitOutput{
			logicalOutputID:    output.LogicalOutputID,
			logicalOutputIndex: output.LogicalOutputIndex,
			packetIndex:        output.PacketIndex,
			packetRole:         output.PacketRole,
			virtualOutputIndex: output.VirtualOutputIndex,
			anchorOutputIndex:  output.AnchorOutputIndex,
			anchorOutpoint:     output.AnchorOutpoint,
			anchorValueSat:     output.AnchorValueSat,
			assetRef:           output.AssetRef,
			issuanceID:         output.IssuanceID,
			amount:             output.Amount,
			taprootAssetRoot:   output.TaprootAssetRoot,
			taprootMerkleRoot:  output.TaprootMerkleRoot,
			scriptKey:          output.ScriptKey,
			scriptMode:         output.ScriptMode,
			opTrueWitness:      witness,
		}
	}
	for idx := range transfer.ProofUpdates {
		update := transfer.ProofUpdates[idx]
		for outputIdx := range result.outputs {
			output := &result.outputs[outputIdx]
			if output.logicalOutputID == update.LogicalOutputID &&
				output.logicalOutputIndex ==
					update.LogicalOutputIndex &&
				output.packetIndex == update.PacketIndex &&
				output.packetRole == update.PacketRole &&
				output.virtualOutputIndex ==
					update.VirtualOutputIndex &&
				output.anchorOutputIndex ==
					update.AnchorOutputIndex &&
				output.anchorOutpoint ==
					update.AnchorOutpoint &&
				output.assetRef.Equivalent(update.AssetRef) &&
				output.issuanceID == update.IssuanceID &&
				output.scriptKey == update.ScriptKey {

				output.proofBlob = append(
					[]byte(nil), update.ProofBlob...,
				)

				break
			}
		}
	}
	for idx := range result.outputs {
		if len(result.outputs[idx].proofBlob) == 0 {
			return nil, fmt.Errorf("tap-sdk output %d has no "+
				"exact proof update", idx)
		}
	}

	return result, nil
}

type commitResponseError struct {
	err error
}

// Error describes a local failure after tapd returned a committed response.
func (e *commitResponseError) Error() string {
	return fmt.Sprintf("process committed tap-sdk response: %v", e.err)
}

// Unwrap exposes the underlying package conversion failure.
func (e *commitResponseError) Unwrap() error {
	return e.err
}
