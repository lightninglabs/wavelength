// Package tapassets adapts tap-sdk custom-anchor transitions to Ark trees.
package tapassets

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/psbt/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/tap-sdk/customanchor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
)

// MaxTransitionInputs bounds the inputs of one issuance that a transition may
// merge. Its proof path step carries the first input as the spine and every
// other input as a co-input path, which tap-sdk limits to
// AssetProofPathMaxStepCoPaths.
const MaxTransitionInputs = 1 + customanchor.ProofPathMaxStepCoPaths

// ErrTooManyTransitionInputs reports a transition whose proof paths tap-sdk
// could not encode.
var ErrTooManyTransitionInputs = errors.New("transition merges too many " +
	"inputs of one issuance")

// checkTransitionInputs rejects a plan that merges more inputs of one issuance
// than MaxTransitionInputs.
func checkTransitionInputs(in []customanchor.AssetInputSummary) error {
	type packetKey struct {
		role  customanchor.PacketRole
		index uint32
	}
	counts := make(map[packetKey]int, len(in))
	for idx := range in {
		input := &in[idx]
		key := packetKey{
			role: input.PacketRole, index: input.PacketIndex,
		}
		counts[key]++
		if counts[key] > MaxTransitionInputs {
			return fmt.Errorf("%w: issuance %x has more than %d",
				ErrTooManyTransitionInputs, input.IssuanceID[:],
				MaxTransitionInputs)
		}
	}

	return nil
}

type commitOutput struct {
	logicalOutputID   string
	packetIndex       uint32
	packetRole        customanchor.PacketRole
	anchorOutputIndex uint32
	anchorOutpoint    tapsdk.Outpoint
	anchorValueSat    int64
	assetRef          tapsdk.AssetRef
	issuanceID        tapsdk.AssetID
	amount            uint64
	taprootAssetRoot  tapsdk.Hash
	taprootMerkleRoot tapsdk.Hash
	scriptKey         tapsdk.PubKey
	scriptMode        customanchor.AssetScriptMode
	opTrueWitness     [][]byte
	proofBlob         []byte
}

type commitInput struct {
	logicalInputID    string
	packetIndex       uint32
	packetRole        customanchor.PacketRole
	virtualInputIndex uint32
	anchorOutpoint    tapsdk.Outpoint
	assetRef          tapsdk.AssetRef
	issuanceID        tapsdk.AssetID
	amount            uint64
	proofSource       commitProofSource
}

type commitProofSource struct {
	kind customanchor.ProofSourceKind
	blob []byte
}

type commitResult struct {
	packageBytes []byte
	anchorPSBT   []byte
	fundingMode  customanchor.FundingMode
	actualFeeSat uint64
	maxFeeSat    uint64
	inputs       []commitInput
	outputs      []commitOutput
}

type assetTreeDriver interface {
	Commit(context.Context, *customanchor.Request,
		customanchor.ConfirmedProofVerifier) (*commitResult, error)

	DecodePackage([]byte) (*commitResult, error)
}

type outputCommitmentPreview struct {
	logicalOutputID   string
	anchorOutputIndex uint32
	assetRoot         tapsdk.Hash
	merkleRoot        tapsdk.Hash
}

type batchAnchorDriver interface {
	assetTreeDriver

	Preview(context.Context, *customanchor.Request,
		customanchor.ConfirmedProofVerifier) (
		[]outputCommitmentPreview,
		error,
	)

	Publish(context.Context, []byte, []byte) error
}

type sdkDriver struct {
	wallet *tapsdk.Wallet
}

// Preview returns the commitment roots produced by a custom-anchor request.
func (d *sdkDriver) Preview(ctx context.Context, request *customanchor.Request,
	verifier customanchor.ConfirmedProofVerifier) (
	[]outputCommitmentPreview, error) {

	if d == nil || d.wallet == nil {
		return nil, fmt.Errorf("tap-sdk wallet is required")
	}

	plan, err := d.buildTransition(ctx, request, verifier)
	if err != nil {
		return nil, err
	}
	previews, err := plan.PreviewOutputCommitments()
	if err != nil {
		return nil, err
	}

	result := make([]outputCommitmentPreview, len(previews))
	for idx := range previews {
		preview := previews[idx]
		result[idx] = outputCommitmentPreview{
			logicalOutputID:   preview.LogicalOutputID,
			anchorOutputIndex: preview.AnchorOutputIndex,
			assetRoot:         preview.TaprootAssetRoot,
			merkleRoot:        preview.TaprootMerkleRoot,
		}
	}

	return result, nil
}

// buildTransition plans a transition whose proof paths can be encoded, before
// tapd seals anything.
func (d *sdkDriver) buildTransition(ctx context.Context,
	request *customanchor.Request,
	verifier customanchor.ConfirmedProofVerifier) (*customanchor.Plan,
	error) {

	builder := customanchor.NewTxBuilder(d.wallet)
	if verifier != nil {
		builder.SetConfirmedProofVerifier(verifier)
	}
	plan, err := builder.Build(ctx, request)
	if err != nil {
		return nil, err
	}
	if err := checkTransitionInputs(plan.Inputs()); err != nil {
		return nil, err
	}

	return plan, nil
}

// Publish verifies and records a finalized custom-anchor transaction.
func (d *sdkDriver) Publish(ctx context.Context, packageBytes,
	finalPSBT []byte) error {

	return d.PublishWithInputProofs(ctx, packageBytes, finalPSBT, nil)
}

// PublishWithInputProofs is Publish for a transfer whose input histories tapd
// may lack, such as a confirmed leaf of an off-chain tree. It supplies the
// inputs' proof files with the transfer.
func (d *sdkDriver) PublishWithInputProofs(ctx context.Context, packageBytes,
	finalPSBT []byte, inputProofs [][]byte) error {

	if d == nil || d.wallet == nil {
		return fmt.Errorf("tap-sdk wallet is required")
	}

	var transfer customanchor.TransferPackage
	if err := transfer.UnmarshalBinary(packageBytes); err != nil {
		return fmt.Errorf("decode sealed transfer package: %w", err)
	}
	if err := transfer.VerifyFinalAnchorPSBT(finalPSBT); err != nil {
		return fmt.Errorf("verify final anchor PSBT: %w", err)
	}

	_, err := customanchor.PublishTransfer(
		ctx, d.wallet, &transfer, finalPSBT,
		customanchor.WithInputProofFiles(inputProofs...),
	)

	return err
}

// Commit builds, commits, verifies, and seals one SDK custom-anchor request.
func (d *sdkDriver) Commit(ctx context.Context, request *customanchor.Request,
	verifier customanchor.ConfirmedProofVerifier) (*commitResult, error) {

	if d == nil || d.wallet == nil {
		return nil, fmt.Errorf("tap-sdk wallet is required")
	}

	plan, err := d.buildTransition(ctx, request, verifier)
	if err != nil {
		return nil, err
	}

	result, err := plan.Commit(ctx, customanchor.CommitOptions{
		Publish: customanchor.PublishMetadata{
			SkipAnchorTxBroadcast: true,
			ExternalBroadcast:     true,
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

// DecodePackage validates and reads a sealed SDK package.
func (d *sdkDriver) DecodePackage(encoded []byte) (*commitResult, error) {
	return decodeSealedPackage(encoded)
}

// decodeSealedPackage validates and reads a sealed SDK package.
func decodeSealedPackage(encoded []byte) (*commitResult, error) {
	var transfer customanchor.TransferPackage
	if err := transfer.UnmarshalBinary(encoded); err != nil {
		return nil, fmt.Errorf("decode tap-sdk transfer package: %w",
			err)
	}

	return commitResultFromPackage(&transfer)
}

// commitResultFromPackage keeps the package fields used by tree construction.
func commitResultFromPackage(transfer *customanchor.TransferPackage) (
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

	return commitResultFromValidatedPackage(transfer, encoded)
}

func commitResultFromValidatedPackage(transfer *customanchor.TransferPackage,
	encoded []byte) (*commitResult, error) {

	result := &commitResult{
		packageBytes: append([]byte(nil), encoded...),
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
			packetIndex:       input.PacketIndex,
			packetRole:        input.PacketRole,
			virtualInputIndex: input.VirtualInputIndex,
			anchorOutpoint:    input.AnchorOutpoint,
			assetRef:          input.AssetRef,
			issuanceID:        input.IssuanceID,
			amount:            input.Amount,
			proofSource: commitProofSource{
				kind: input.ProofSource.Kind,
				blob: append(
					[]byte(nil), input.ProofSource.Blob...,
				),
			},
		}
	}

	proofs := make(map[proofUpdateKey][]byte, len(transfer.ProofUpdates))
	for idx := range transfer.ProofUpdates {
		update := transfer.ProofUpdates[idx]
		key := proofUpdateKey{
			packetRole:         update.PacketRole,
			packetIndex:        update.PacketIndex,
			virtualOutputIndex: update.VirtualOutputIndex,
		}
		proofs[key] = update.ProofBlob
	}

	for idx := range transfer.Outputs {
		output := transfer.Outputs[idx]
		var witness [][]byte
		if output.OPTrueSpend != nil {
			witness = output.OPTrueSpend.WitnessStack()
		}
		key := proofUpdateKey{
			packetRole:         output.PacketRole,
			packetIndex:        output.PacketIndex,
			virtualOutputIndex: output.VirtualOutputIndex,
		}
		proofBlob := proofs[key]
		if len(proofBlob) == 0 {
			return nil, fmt.Errorf("tap-sdk output %d has no "+
				"proof update", idx)
		}

		result.outputs[idx] = commitOutput{
			logicalOutputID:   output.LogicalOutputID,
			packetIndex:       output.PacketIndex,
			packetRole:        output.PacketRole,
			anchorOutputIndex: output.AnchorOutputIndex,
			anchorOutpoint:    output.AnchorOutpoint,
			anchorValueSat:    output.AnchorValueSat,
			assetRef:          output.AssetRef,
			issuanceID:        output.IssuanceID,
			amount:            output.Amount,
			taprootAssetRoot:  output.TaprootAssetRoot,
			taprootMerkleRoot: output.TaprootMerkleRoot,
			scriptKey:         output.ScriptKey,
			scriptMode:        output.ScriptMode,
			opTrueWitness:     witness,
			proofBlob: append(
				[]byte(nil), proofBlob...,
			),
		}
	}

	return result, nil
}

type proofUpdateKey struct {
	packetRole         customanchor.PacketRole
	packetIndex        uint32
	virtualOutputIndex uint32
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

// CommitOnboarding commits a custom anchor that tap-sdk itself will publish
// after Wavelength supplies the final Bitcoin signature.
func (d *sdkDriver) CommitOnboarding(ctx context.Context,
	request *customanchor.Request) (*commitResult, error) {

	if d == nil || d.wallet == nil {
		return nil, fmt.Errorf("tap-sdk wallet is required")
	}

	builder := customanchor.NewTxBuilder(d.wallet)
	plan, err := builder.Build(ctx, request)
	if err != nil {
		return nil, err
	}

	result, err := plan.Commit(ctx, customanchor.CommitOptions{
		Publish: customanchor.PublishMetadata{
			Label: "wavelength-onboarding",
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

// VerifyFinalOnboarding validates Wavelength's exact signed PSBT against the
// sealed tap-sdk package before either publishing or restoring it.
func (d *sdkDriver) VerifyFinalOnboarding(packageBytes,
	finalPSBT []byte) error {

	var transfer customanchor.TransferPackage
	if err := transfer.UnmarshalBinary(packageBytes); err != nil {
		return fmt.Errorf("decode tap-sdk transfer package: %w", err)
	}
	if err := transfer.VerifyFinalAnchorPSBT(finalPSBT); err != nil {
		return fmt.Errorf("verify final onboarding anchor PSBT: %w",
			err)
	}

	return nil
}

// errTransferNotRecorded reports that tapd holds no transfer for a signed
// transaction.
var errTransferNotRecorded = errors.New("tapd has no recorded transfer")

// ReconcileOnboarding requires tapd to have recorded the exact saved signed
// transaction. Absence is ambiguous: an earlier RPC may still be in flight.
func (d *sdkDriver) ReconcileOnboarding(ctx context.Context,
	finalPSBT []byte) error {

	packet, err := psbtutil.Parse(finalPSBT)
	if err != nil {
		return err
	}
	finalTx, err := psbt.Extract(packet)
	if err != nil {
		return err
	}
	txid := finalTx.TxHash()
	transfers, err := d.wallet.Client().ListTransfers(
		ctx, &tapsdk.ListTransfersRequest{
			AnchorTxid: txid.String(),
		},
	)
	if err != nil {
		return errors.Join(ErrReconciliationRequired, err)
	}
	want := serializeTx(finalTx)
	for _, transfer := range transfers {
		if transfer == nil || transfer.TransferTxid != [32]byte(txid) {
			continue
		}
		if !bytes.Equal(transfer.AnchorTx, want) {
			return fmt.Errorf("%w: recorded onboarding "+
				"transaction differs",
				ErrReconciliationRequired)
		}

		return nil
	}

	return fmt.Errorf("%w: %w: %s", ErrReconciliationRequired,
		errTransferNotRecorded, txid)
}
