package assets

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/taprpc/assetwalletrpc"
	"google.golang.org/grpc"
)

// -----------------------------------------------------------------------------
// Transfer Data Types and Helpers
// -----------------------------------------------------------------------------

// TransferData contains the serialized data needed to call
// PublishAndLogTransfer after broadcasting externally, or to construct proofs
// from committed state.
type TransferData struct {
	// AnchorPsbt is the serialized anchor PSBT (signed or finalized).
	AnchorPsbt []byte

	// VirtualPsbts are the serialized active virtual PSBTs with proofs.
	VirtualPsbts [][]byte

	// PassivePsbts are serialized passive virtual PSBTs (if any).
	PassivePsbts [][]byte

	// ChangeOutputIndex is the index of the change output, or -1 if none.
	ChangeOutputIndex int32

	// V1InputTxWitnesses optionally carries the complete virtual
	// transaction witness stacks for each asset input. This is required for
	// V1 assets because TxWitness is "strippable" and may be missing from
	// tapd-generated proof material.
	//
	// When present, BuildProofFromTransferData will prefer these witnesses
	// over attempting to infer them from the input proofs.
	V1InputTxWitnesses []wire.TxWitness
}

// GetTransferData extracts the serialized data needed to call
// PublishAndLogTransfer after broadcasting externally. This should be called
// after Commit() and before or after signing.
func (b *AssetTxBuilder) GetTransferData() (*TransferData, error) {
	if b.anchorPsbt == nil || b.commitResp == nil {
		return nil, errors.New("commit must be called before " +
			"GetTransferData")
	}

	var anchorBuf bytes.Buffer
	if err := b.anchorPsbt.Serialize(&anchorBuf); err != nil {
		return nil, fmt.Errorf("serialize anchor psbt: %w", err)
	}

	virtualBytes := make([][]byte, len(b.activePkts))
	for i := range b.activePkts {
		encoded, err := tappsbt.Encode(b.activePkts[i])
		if err != nil {
			return nil, fmt.Errorf("encode active vpacket %d: %w",
				i, err)
		}
		virtualBytes[i] = encoded
	}

	passiveBytes := make([][]byte, len(b.passivePkts))
	for i := range b.passivePkts {
		encoded, err := tappsbt.Encode(b.passivePkts[i])
		if err != nil {
			return nil, fmt.Errorf("encode passive vpacket %d: %w",
				i, err)
		}
		passiveBytes[i] = encoded
	}

	// Record any V1 input witnesses we can deterministically derive from
	// the builder's static key inputs. This ensures proof materialization
	// can be performed without relying on tapd to preserve script key
	// derivation metadata in the proof.
	v1InputWits := make([]wire.TxWitness, len(b.inputs))
	for i := range b.inputs {
		cfg := b.inputs[i].cfg
		if cfg.AnchorKey.Mode != AnchorKeyModeStatic {
			continue
		}
		if len(cfg.AnchorKey.Key) != schnorr.PubKeyBytesLen {
			continue
		}
		if len(cfg.ProofFile) == 0 {
			continue
		}

		internalKey, err := schnorr.ParsePubKey(cfg.AnchorKey.Key)
		if err != nil {
			continue
		}

		// Only attach OP_TRUE witness if input proof indicates the
		// previous output is an OP_TRUE script key spend for this
		// key.
		artifacts, err := BuildOpTrueArtifacts(internalKey)
		if err != nil {
			continue
		}

		inputFile, err := proof.DecodeFile(cfg.ProofFile)
		if err != nil {
			continue
		}
		inputPf, err := inputFile.LastProof()
		if err != nil {
			continue
		}

		artifactsKeyX := schnorr.SerializePubKey(artifacts.OutputKey)
		inputScriptKeyX := schnorr.SerializePubKey(
			inputPf.Asset.ScriptKey.PubKey,
		)
		if bytes.Equal(artifactsKeyX, inputScriptKeyX) {
			v1InputWits[i] = copyWitness(artifacts.Witness)
		}
	}

	return &TransferData{
		AnchorPsbt:         anchorBuf.Bytes(),
		VirtualPsbts:       virtualBytes,
		PassivePsbts:       passiveBytes,
		ChangeOutputIndex:  b.commitResp.GetChangeOutputIndex(),
		V1InputTxWitnesses: v1InputWits,
	}, nil
}

// -----------------------------------------------------------------------------
// V1 Witness Helpers
// -----------------------------------------------------------------------------

// opTrueWitnessFromProof determines the correct OP_TRUE witness for a V1
// asset spend based on its input proof. For standard OP_TRUE outputs (using
// NUMS), it returns the NUMS-based witness. For OpTrueUniqueScript outputs,
// it returns a witness built from the internal key found in TweakedScriptKey
// or InclusionProof.InternalKey.
// Returns nil if the witness cannot be determined from the proof.
func opTrueWitnessFromProof(proofData []byte) wire.TxWitness {
	if len(proofData) == 0 {
		return nil
	}

	inputFile, err := proof.DecodeFile(proofData)
	if err != nil {
		return nil
	}

	inputPf, err := inputFile.LastProof()
	if err != nil {
		return nil
	}

	inputScriptKey := inputPf.Asset.ScriptKey.PubKey
	if inputScriptKey == nil {
		return nil
	}

	// Build NUMS-based OP_TRUE and check if it matches the input's
	// script key.
	numsArtifacts, err := BuildOpTrueArtifacts(asset.NUMSPubKey)
	if err == nil {
		numsKeyX := schnorr.SerializePubKey(numsArtifacts.OutputKey)
		inputKeyX := schnorr.SerializePubKey(inputScriptKey)
		if bytes.Equal(numsKeyX, inputKeyX) {
			// Input uses standard OP_TRUE (NUMS).
			return numsArtifacts.Witness
		}
	}

	// Check if input uses OpTrueUniqueScript. First try TweakedScriptKey
	// which has the exact internal key used to derive the script key.
	tweakedInfo := inputPf.Asset.ScriptKey.TweakedScriptKey
	if tweakedInfo != nil && tweakedInfo.RawKey.PubKey != nil {
		internalKey := tweakedInfo.RawKey.PubKey
		artifacts, err := BuildOpTrueArtifacts(internalKey)
		if err == nil {
			artifactsKeyX := schnorr.SerializePubKey(
				artifacts.OutputKey,
			)
			inputKeyX := schnorr.SerializePubKey(inputScriptKey)
			if bytes.Equal(artifactsKeyX, inputKeyX) {
				return artifacts.Witness
			}
		}
	}

	// Fallback: try InclusionProof.InternalKey. For OpTrueUniqueScript
	// outputs where the same key is used for both anchor and script,
	// this will work. We verify the script key matches to ensure this
	// is actually an OP_TRUE output.
	internalKey := inputPf.InclusionProof.InternalKey
	if internalKey == nil {
		return nil
	}

	artifacts, err := BuildOpTrueArtifacts(internalKey)
	if err != nil {
		return nil
	}

	// Verify the computed OP_TRUE output key matches the input's script
	// key. If they don't match, this is not an OP_TRUE input.
	// NOTE: The internal key from InclusionProof may have a different
	// prefix (02 vs 03) than what we use for OP_TRUE.
	artifactsKeyX := schnorr.SerializePubKey(artifacts.OutputKey)
	inputKeyX := schnorr.SerializePubKey(inputScriptKey)
	if !bytes.Equal(artifactsKeyX, inputKeyX) {
		return nil
	}

	return artifacts.Witness
}

// virtualPsbtSigner is a narrow interface for signing Taproot Asset virtual
// PSBTs. This is used to obtain input witnesses for tapd-managed script keys
// when constructing proofs without relying on tapd's ExportProof RPC.
type virtualPsbtSigner interface {
	FundVirtualPsbt(context.Context, *assetwalletrpc.FundVirtualPsbtRequest,
		...grpc.CallOption) (*assetwalletrpc.FundVirtualPsbtResponse,
		error)

	SignVirtualPsbt(context.Context, *assetwalletrpc.SignVirtualPsbtRequest,
		...grpc.CallOption) (*assetwalletrpc.SignVirtualPsbtResponse,
		error)
}

// SignVirtualPackets signs all active virtual PSBTs with tapd.
//
// This is required when spending tapd-managed script keys, because the virtual
// transaction witness (a Schnorr signature) is not known to the caller. For V1
// assets, these witnesses are strippable and must be re-attached during proof
// construction for TAP VM validation.
func (b *AssetTxBuilder) SignVirtualPackets(ctx context.Context,
	signer virtualPsbtSigner) error {

	if signer == nil {
		return errors.New("virtual psbt signer missing")
	}
	if len(b.activePkts) == 0 {
		return errors.New("no active packets - call Commit first")
	}

	for i := range b.activePkts {
		encoded, err := tappsbt.Encode(b.activePkts[i])
		if err != nil {
			return fmt.Errorf("encode active vpacket %d: %w",
				i, err)
		}

		tmpl := &assetwalletrpc.FundVirtualPsbtRequest_Psbt{
			Psbt: encoded,
		}
		fundResp, err := signer.FundVirtualPsbt(
			ctx, &assetwalletrpc.FundVirtualPsbtRequest{
				Template: tmpl,
			},
		)
		if err != nil {
			return fmt.Errorf("fund virtual psbt %d: %w", i, err)
		}

		resp, err := signer.SignVirtualPsbt(
			ctx, &assetwalletrpc.SignVirtualPsbtRequest{
				FundedPsbt: fundResp.FundedPsbt,
			},
		)
		if err != nil {
			return fmt.Errorf("sign virtual psbt %d: %w", i, err)
		}

		signedPkt, err := tappsbt.NewFromRawBytes(
			bytes.NewReader(resp.SignedPsbt), false,
		)
		if err != nil {
			return fmt.Errorf("decode signed vpacket %d: %w",
				i, err)
		}

		// The signed packet returned by tapd is not guaranteed to
		// carry the proof suffix from CommitVirtualPsbts. Merge
		// only the signed input and output witness data we need
		// into the builder's packet.
		for _, inputIdx := range resp.SignedInputs {
			idx := int(inputIdx)
			if idx < 0 || idx >= len(b.activePkts[i].Inputs) ||
				idx >= len(signedPkt.Inputs) {

				return fmt.Errorf("signed input index %d "+
					"out of range", idx)
			}

			origIn := &b.activePkts[i].Inputs[idx].PInput
			signedIn := signedPkt.Inputs[idx].PInput

			origIn.TaprootKeySpendSig = append(
				[]byte(nil), signedIn.TaprootKeySpendSig...,
			)
			scriptSpendSig := signedIn.TaprootScriptSpendSig
			origIn.TaprootScriptSpendSig = scriptSpendSig
			origIn.TaprootLeafScript = signedIn.TaprootLeafScript
			origIn.PartialSigs = signedIn.PartialSigs
			origIn.FinalScriptSig = append(
				[]byte(nil), signedIn.FinalScriptSig...,
			)
			origIn.FinalScriptWitness = append(
				[]byte(nil), signedIn.FinalScriptWitness...,
			)
		}

		for outIdx := range b.activePkts[i].Outputs {
			if outIdx >= len(signedPkt.Outputs) {
				break
			}

			origOut := b.activePkts[i].Outputs[outIdx]
			signedOut := signedPkt.Outputs[outIdx]

			if origOut.Asset == nil || signedOut.Asset == nil {
				continue
			}

			signedPrevWits := signedOut.Asset.PrevWitnesses
			for witIdx := range origOut.Asset.PrevWitnesses {
				if witIdx >= len(signedPrevWits) {
					break
				}

				signedWit := signedPrevWits[witIdx].TxWitness
				if len(signedWit) == 0 {
					continue
				}

				origOut.Asset.PrevWitnesses[witIdx].TxWitness =
					copyWitness(signedWit)
			}
		}
	}

	return nil
}

// -----------------------------------------------------------------------------
// Proof Construction
// -----------------------------------------------------------------------------

// ProofParams contains the chain-level information needed to finalize a proof.
// This is used by the Proof() method to construct a complete proof file after
// the anchor transaction has been confirmed.
type ProofParams struct {
	// Block is the confirmed block containing the anchor transaction.
	Block *wire.MsgBlock

	// BlockHeight is the height of the confirmed block.
	BlockHeight uint32

	// TxIndex is the index of the anchor transaction within the block.
	TxIndex int

	// InternalKey optionally overrides InclusionProof.InternalKey. This is
	// needed when the actual anchor output uses a different internal
	// key than what tapd's virtual PSBT references (e.g., tree nodes with
	// per-child MuSig2 keys).
	InternalKey *btcec.PublicKey

	// PrevOut optionally overrides the proof's PrevOut field. This is the
	// outpoint that the proof's transaction is spending. Needed when the
	// actual spent outpoint differs from what tapd's virtual PSBT expects.
	PrevOut *wire.OutPoint

	// TapSiblingPreimage optionally overrides the proof's
	// TapSiblingPreimage. This is the preimage of the tapscript sibling
	// (e.g., sweep script) that is hashed together with the asset
	// commitment to form the taproot tree. Needed when the tree node uses
	// a different sweep script than what tapd originally created.
	TapSiblingPreimage *commitment.TapscriptPreimage
}

// Proof returns a complete proof file for the specified output index. This
// method constructs a valid proof chain that can be imported into tapd's
// universe for proof chain continuity.
//
// The method handles:
//   - Extracting the proof suffix from the committed virtual packet
//   - Updating PrevWitnesses with correct outpoints from input proofs
//   - For V1 assets, populating missing TxWitnesses (strippable witnesses)
//   - Appending to the base proof file from the first input
//   - Including AdditionalInputs for multi-input transfers
//   - Updating with confirmation data if ProofParams is provided
//
// This method must be called after Commit() and FinalizeAnchor(). If params
// is nil, the proof will not include confirmation data (block, tx merkle
// proof) and will need to be updated later via UpdateTransitionProof.
func (b *AssetTxBuilder) Proof(outputIndex int,
	params *ProofParams) ([]byte, error) {

	// Validate builder state.
	if len(b.activePkts) == 0 {
		return nil, errors.New("no active packets - call Commit first")
	}

	if b.anchorPsbt == nil {
		return nil, errors.New(
			"anchor PSBT not set - call Commit first",
		)
	}

	// Validate output index bounds.
	if outputIndex < 0 || outputIndex >= len(b.activePkts[0].Outputs) {
		return nil, fmt.Errorf("output index %d out of range [0, %d)",
			outputIndex, len(b.activePkts[0].Outputs))
	}

	// Get the proof suffix for this output.
	vOut := b.activePkts[0].Outputs[outputIndex]
	if vOut.ProofSuffix == nil {
		return nil, fmt.Errorf("output %d has no proof suffix",
			outputIndex)
	}

	proofEntry := vOut.ProofSuffix

	// Get the base proof file from the first input.
	if len(b.inputs) == 0 || len(b.inputs[0].cfg.ProofFile) == 0 {
		return nil, errors.New("no input proofs available")
	}

	baseProofFile, err := proof.DecodeFile(b.inputs[0].cfg.ProofFile)
	if err != nil {
		return nil, fmt.Errorf("decode base proof file: %w", err)
	}

	// Collect additional input proofs for multi-input transfers.
	additionalInputs := make([]proof.File, 0, len(b.inputs)-1)
	for i := 1; i < len(b.inputs); i++ {
		if len(b.inputs[i].cfg.ProofFile) == 0 {
			continue
		}
		decoded, err := proof.DecodeFile(b.inputs[i].cfg.ProofFile)
		if err != nil {
			return nil, fmt.Errorf("decode input proof %d: %w",
				i, err)
		}
		additionalInputs = append(additionalInputs, *decoded)
	}

	// Copy the proof's asset so we can modify PrevWitnesses (TxWitness).
	// Note: We do NOT modify PrevID because:
	// 1. For split outputs (ZeroPrevID), the PrevID was already correctly
	//    set by NewSplitCommitment and modifying would break the leaf hash.
	// 2. For all other outputs (including split roots), the PrevID was
	//    already correctly set by tapd based on the input proofs.
	// Modifying PrevID would cause a mismatch with the committed asset
	// because the merkle proof in CommitmentProof was computed for the
	// original PrevID values.
	proofAsset := proofEntry.Asset.Copy()

	// For V1 assets, populate any missing TxWitnesses. V1 assets have
	// "strippable" witnesses (similar to SegWit) that are not included in
	// the TAP commitment, so we can freely set them for VM validation.
	//
	// For OP_TRUE inputs, we generate the appropriate witness based on the
	// input proof. For non-OP_TRUE inputs (like tapd-managed BIP86 keys),
	// we use the witness from vOut.Asset which tapd populated during
	// CommitVirtualPsbts (the ProofSuffix.Asset may have stripped witness).
	if proofAsset.Version == asset.V1 {
		for i := range proofAsset.PrevWitnesses {
			var proofData []byte
			if i < len(b.inputs) {
				proofData = b.inputs[i].cfg.ProofFile
			}

			// Try to get OP_TRUE witness from input proof.
			witness := opTrueWitnessFromProof(proofData)
			if len(witness) > 0 {
				// Input is OP_TRUE - use generated witness.
				proofAsset.PrevWitnesses[i].TxWitness =
					copyWitness(witness)

				continue
			}

			// Input is not OP_TRUE. Try to get witness from
			// vOut.Asset which tapd may have populated during
			// CommitVirtualPsbts. The ProofSuffix.Asset may have
			// the witness stripped for V1 assets.
			if vOut.Asset != nil &&
				i < len(vOut.Asset.PrevWitnesses) &&
				len(vOut.Asset.PrevWitnesses[i].TxWitness) > 0 {

				proofAsset.PrevWitnesses[i].TxWitness =
					copyWitness(
						vOut.Asset.PrevWitnesses[i].
							TxWitness,
					)
			}
		}
	}

	// Extract the anchor transaction.
	anchorTx, err := psbt.Extract(b.anchorPsbt)
	if err != nil {
		return nil, fmt.Errorf("extract anchor tx: %w", err)
	}

	// Update the proof entry with the patched asset.
	proofEntry.Asset = *proofAsset
	proofEntry.AdditionalInputs = additionalInputs
	proofEntry.AnchorTx = *anchorTx

	// Set the output index from the virtual output's anchor output index.
	// This is critical for split transactions where each virtual output
	// maps to a different anchor output. The proof suffix from tapd may
	// have the virtual output index rather than the anchor output index.
	proofEntry.InclusionProof.OutputIndex = vOut.AnchorOutputIndex

	// Update with confirmation data if provided.
	if params != nil {
		err = proofEntry.UpdateTransitionProof(&proof.BaseProofParams{
			Block:       params.Block,
			BlockHeight: params.BlockHeight,
			Tx:          anchorTx,
			TxIndex:     params.TxIndex,
		})
		if err != nil {
			return nil, fmt.Errorf(
				"update transition proof: %w", err,
			)
		}
	}

	// Append to the base proof file.
	if err := baseProofFile.AppendProof(*proofEntry); err != nil {
		return nil, fmt.Errorf("append proof: %w", err)
	}

	// Encode the complete proof file.
	var buf bytes.Buffer
	if err := baseProofFile.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode proof file: %w", err)
	}

	return buf.Bytes(), nil
}

// BuildProofFromTransferData constructs a complete proof file from serialized
// transfer data. This is useful for building proofs after a transaction has
// been broadcast externally, when only the TransferData (from GetTransferData)
// is available rather than the full builder state.
//
// Parameters:
//   - td: The TransferData containing serialized VirtualPsbts and AnchorPsbt
//   - inputProofs: Proof files for each input being spent (for PrevWitness
//     outpoints)
//   - outputIndex: Which output's proof to build (within the first virtual
//     PSBT)
//   - params: Optional confirmation data (block, height, tx index)
//
// The function handles the same proof construction as builder.Proof():
//   - Extracting ProofSuffix from the virtual PSBT
//   - Updating PrevWitnesses with correct outpoints
//   - For V1 assets, populating missing TxWitnesses
//   - Appending to the base proof file
//   - Including confirmation data if provided
//
//nolint:funlen
func BuildProofFromTransferData(td *TransferData, inputProofs [][]byte,
	outputIndex int, params *ProofParams) ([]byte, error) {

	if td == nil {
		return nil, errors.New("transfer data is nil")
	}

	if len(td.VirtualPsbts) == 0 {
		return nil, errors.New("no virtual PSBTs in transfer data")
	}

	if len(inputProofs) == 0 {
		return nil, errors.New("no input proofs provided")
	}

	// Decode the first virtual PSBT to get the proof suffix.
	vPkt, err := tappsbt.Decode(td.VirtualPsbts[0])
	if err != nil {
		return nil, fmt.Errorf("decode virtual PSBT: %w", err)
	}

	// Validate output index.
	if outputIndex < 0 || outputIndex >= len(vPkt.Outputs) {
		return nil, fmt.Errorf("output index %d out of range [0, %d)",
			outputIndex, len(vPkt.Outputs))
	}

	vOut := vPkt.Outputs[outputIndex]
	if vOut.ProofSuffix == nil {
		return nil, fmt.Errorf("output %d has no proof suffix",
			outputIndex)
	}

	proofEntry := vOut.ProofSuffix

	// Decode the base proof file from the first input.
	baseProofFile, err := proof.DecodeFile(inputProofs[0])
	if err != nil {
		return nil, fmt.Errorf("decode base proof file: %w", err)
	}

	// Collect additional input proofs for multi-input transfers.
	additionalInputs := make([]proof.File, 0, len(inputProofs)-1)
	for i := 1; i < len(inputProofs); i++ {
		if len(inputProofs[i]) == 0 {
			continue
		}
		decoded, err := proof.DecodeFile(inputProofs[i])
		if err != nil {
			return nil, fmt.Errorf("decode input proof %d: %w",
				i, err)
		}
		additionalInputs = append(additionalInputs, *decoded)
	}

	// Copy the proof's asset so we can modify PrevWitnesses (TxWitness).
	// Note: We do NOT modify PrevID because:
	// 1. For split outputs (ZeroPrevID), the PrevID was already correctly
	//    set by NewSplitCommitment and modifying would break the leaf hash.
	// 2. For all other outputs (including split roots), the PrevID was
	//    already correctly set by tapd based on the input proofs.
	// Modifying PrevID would cause a mismatch with the committed asset
	// because the merkle proof in CommitmentProof was computed for the
	// original PrevID values.
	proofAsset := proofEntry.Asset.Copy()

	// For V1 assets, populate any missing TxWitnesses. V1 assets have
	// "strippable" witnesses (similar to SegWit) that are not included in
	// the TAP commitment, so we can freely set them for VM validation.
	//
	// For OP_TRUE inputs, we generate the appropriate witness based on the
	// input proof. For non-OP_TRUE inputs (like tapd-managed BIP86 keys),
	// we preserve the witness from the ProofSuffix that tapd populated.
	//
	// IMPORTANT: Skip witnesses that have SplitCommitment set - these are
	// split output witnesses that should NOT have TxWitness. Setting
	// TxWitness on them would break IsSplitCommitWitness() which checks
	// len(TxWitness) == 0.
	if proofAsset.Version == asset.V1 {
		for i := range proofAsset.PrevWitnesses {
			// Skip split output witnesses - they have
			// SplitCommitment and must NOT have TxWitness for
			// IsSplitCommitWitness() to return true.
			if proofAsset.PrevWitnesses[i].SplitCommitment != nil {
				continue
			}

			// Prefer the witness the builder recorded at commit
			// time (if available). This avoids relying on proof
			// metadata that tapd might not preserve.
			if i < len(td.V1InputTxWitnesses) &&
				len(td.V1InputTxWitnesses[i]) > 0 {

				proofAsset.PrevWitnesses[i].TxWitness =
					copyWitness(td.V1InputTxWitnesses[i])
				continue
			}

			var proofData []byte
			if i < len(inputProofs) {
				proofData = inputProofs[i]
			}

			// Try to get OP_TRUE witness from input proof.
			witness := opTrueWitnessFromProof(proofData)
			if len(witness) > 0 {
				// Input is OP_TRUE - use generated witness.
				proofAsset.PrevWitnesses[i].TxWitness =
					copyWitness(witness)
			}
			// If input is not OP_TRUE, preserve existing witness
			// from ProofSuffix.
		}
	}

	// For split outputs (those with SplitCommitment), we also need to
	// populate the TxWitness on the ROOT asset inside SplitCommitment.
	// The verifier extracts the root asset and validates its witnesses.
	hasSplitWitness := proofAsset.HasSplitCommitmentWitness()
	if proofAsset.Version == asset.V1 && hasSplitWitness {
		splitCommit := proofAsset.PrevWitnesses[0].SplitCommitment
		rootAsset := &splitCommit.RootAsset
		for i := range rootAsset.PrevWitnesses {
			if i < len(td.V1InputTxWitnesses) &&
				len(td.V1InputTxWitnesses[i]) > 0 {

				rootAsset.PrevWitnesses[i].TxWitness =
					copyWitness(td.V1InputTxWitnesses[i])
				continue
			}

			var proofData []byte
			if i < len(inputProofs) {
				proofData = inputProofs[i]
			}

			// Try to get OP_TRUE witness from input proof.
			witness := opTrueWitnessFromProof(proofData)
			if len(witness) > 0 {
				// Input is OP_TRUE - use generated witness.
				rootAsset.PrevWitnesses[i].TxWitness =
					copyWitness(witness)
			}
			// If input is not OP_TRUE, preserve existing witness.
		}
	}

	// Extract the anchor transaction from the serialized PSBT.
	anchorPsbt, err := psbt.NewFromRawBytes(
		bytes.NewReader(td.AnchorPsbt), false,
	)
	if err != nil {
		return nil, fmt.Errorf("decode anchor PSBT: %w", err)
	}

	anchorTx, err := psbt.Extract(anchorPsbt)
	if err != nil {
		return nil, fmt.Errorf("extract anchor tx: %w", err)
	}

	// Update the proof entry with the patched asset.
	proofEntry.Asset = *proofAsset
	proofEntry.AdditionalInputs = additionalInputs
	proofEntry.AnchorTx = *anchorTx

	// Set the output index from the virtual output's anchor output index.
	// This is critical for split transactions where each virtual output
	// maps to a different anchor output. The proof suffix from tapd may
	// have the virtual output index rather than the anchor output index.
	proofEntry.InclusionProof.OutputIndex = vOut.AnchorOutputIndex

	// Override internal key if provided. This is critical for tree nodes
	// where each child output has a different MuSig2 internal key based on
	// its cosigners. Fall back to AnchorOutputInternalKey if no override is
	// provided but the output has a custom internal key.
	if params != nil && params.InternalKey != nil {
		proofEntry.InclusionProof.InternalKey = params.InternalKey
	} else if vOut.AnchorOutputInternalKey != nil {
		anchorInternalKey := vOut.AnchorOutputInternalKey
		proofEntry.InclusionProof.InternalKey = anchorInternalKey
	}

	// Override PrevOut if provided. This is the outpoint being spent, which
	// may differ from what tapd's virtual PSBT expects when spending tree
	// nodes.
	if params != nil && params.PrevOut != nil {
		proofEntry.PrevOut = *params.PrevOut
	}

	// Override TapSiblingPreimage if provided. This is needed when the tree
	// node uses a different sweep script than what tapd originally created.
	if params != nil && params.TapSiblingPreimage != nil {
		commitProof := proofEntry.InclusionProof.CommitmentProof
		if commitProof != nil {
			siblingPreimage := params.TapSiblingPreimage
			commitProof.TapSiblingPreimage = siblingPreimage
		}
	}

	// Update with confirmation data if provided.
	if params != nil && params.Block != nil {
		err = proofEntry.UpdateTransitionProof(&proof.BaseProofParams{
			Block:       params.Block,
			BlockHeight: params.BlockHeight,
			Tx:          anchorTx,
			TxIndex:     params.TxIndex,
		})
		if err != nil {
			return nil, fmt.Errorf(
				"update transition proof: %w", err,
			)
		}
	}

	// Append to the base proof file.
	if err := baseProofFile.AppendProof(*proofEntry); err != nil {
		return nil, fmt.Errorf("append proof: %w", err)
	}

	// Encode the complete proof file.
	var buf bytes.Buffer
	if err := baseProofFile.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode proof file: %w", err)
	}

	return buf.Bytes(), nil
}
