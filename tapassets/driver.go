// Package tapassets adapts tap-sdk custom-anchor transitions to Ark trees.
package tapassets

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/psbt/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
)

type commitOutput struct {
	logicalOutputID   string
	packetIndex       uint32
	packetRole        tapsdk.CustomAnchorPacketRole
	anchorOutputIndex uint32
	anchorOutpoint    tapsdk.Outpoint
	anchorValueSat    int64
	assetRef          tapsdk.AssetRef
	issuanceID        tapsdk.AssetID
	amount            uint64
	taprootAssetRoot  tapsdk.Hash
	taprootMerkleRoot tapsdk.Hash
	scriptKey         tapsdk.PubKey
	scriptMode        tapsdk.CustomAssetScriptMode
	opTrueWitness     [][]byte
	proofBlob         []byte
}

type commitInput struct {
	logicalInputID    string
	packetIndex       uint32
	packetRole        tapsdk.CustomAnchorPacketRole
	virtualInputIndex uint32
	anchorOutpoint    tapsdk.Outpoint
	assetRef          tapsdk.AssetRef
	issuanceID        tapsdk.AssetID
	amount            uint64
	proofSource       commitProofSource
}

type commitProofSource struct {
	kind tapsdk.CustomAnchorProofSourceKind
	blob []byte
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

type assetTreeDriver interface {
	Commit(context.Context, *tapsdk.CustomAnchorRequest,
		tapsdk.ConfirmedProofVerifier) (*commitResult, error)

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

	Preview(context.Context, *tapsdk.CustomAnchorRequest,
		tapsdk.ConfirmedProofVerifier) (
		[]outputCommitmentPreview,
		error,
	)

	Publish(context.Context, []byte, []byte) error
}

type sdkDriver struct {
	wallet *tapsdk.Wallet
}

// Preview returns the commitment roots produced by a custom-anchor request.
func (d *sdkDriver) Preview(ctx context.Context,
	request *tapsdk.CustomAnchorRequest,
	verifier tapsdk.ConfirmedProofVerifier) ([]outputCommitmentPreview,
	error) {

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

// Publish verifies and records a finalized custom-anchor transaction.
func (d *sdkDriver) Publish(ctx context.Context, packageBytes,
	finalPSBT []byte) error {

	if d == nil || d.wallet == nil {
		return fmt.Errorf("tap-sdk wallet is required")
	}

	var transfer tapsdk.CustomAnchorTransferPackage
	if err := transfer.UnmarshalBinary(packageBytes); err != nil {
		return fmt.Errorf("decode sealed transfer package: %w", err)
	}
	if err := transfer.VerifyFinalAnchorPSBT(finalPSBT); err != nil {
		return fmt.Errorf("verify final anchor PSBT: %w", err)
	}

	_, err := d.wallet.PublishCustomAnchorTransfer(
		ctx, &transfer, finalPSBT,
	)

	return err
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
	var transfer tapsdk.CustomAnchorTransferPackage
	if err := transfer.UnmarshalBinary(encoded); err != nil {
		return nil, fmt.Errorf("decode tap-sdk transfer package: %w",
			err)
	}

	return commitResultFromPackage(&transfer)
}

// commitResultFromPackage keeps the package fields used by tree construction.
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

	return commitResultFromValidatedPackage(transfer, encoded)
}

func commitResultFromValidatedPackage(
	transfer *tapsdk.CustomAnchorTransferPackage,
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
	packetRole         tapsdk.CustomAnchorPacketRole
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

	return fmt.Errorf("%w: tapd has no recorded onboarding transfer for %s",
		ErrReconciliationRequired, txid)
}
