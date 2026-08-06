package tapassets

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
)

// BatchAnchorSource identifies the confirmed asset funding UTXO a commitment
// transition spends: its proof, the Bitcoin outpoint anchoring it (which
// must be an input of the anchor transaction), and the tapd-owned internal
// key authorizing that input's key spend.
type BatchAnchorSource struct {
	// ProofFile is the funding UTXO's complete confirmed proof file.
	ProofFile []byte

	// Witness is the caller-provided asset witness stack. Empty selects
	// tapd's backend signer, for funding UTXOs whose asset script key is
	// wallet-owned.
	Witness [][]byte

	// Verifier verifies the funding UTXO proof when building each commit.
	Verifier tapsdk.ConfirmedProofVerifier

	// AnchorOutpoint is the Bitcoin outpoint anchoring the funding UTXO.
	AnchorOutpoint wire.OutPoint

	// AnchorInternalKey is the tapd anchor internal key that signs the
	// funding input's key spend.
	AnchorInternalKey *btcec.PublicKey
}

// BatchAnchorRequest describes one asset batch output on a caller-funded
// anchor transaction: the whole funding UTXO moves into a single output whose
// taproot key is the cosigner aggregate tweaked with the combined tapscript
// root. Full-value transitions produce no split commitment, so the derived
// roots are independent of output reordering by the funding wallet.
type BatchAnchorRequest struct {
	// AssetRef identifies the asset carried by the batch output.
	AssetRef tapsdk.AssetRef

	// Amount is the funding UTXO's full asset amount; the batch output
	// carries all of it.
	Amount uint64

	// Source identifies the funding UTXO the transition spends.
	Source BatchAnchorSource

	// Cosigners aggregate into the batch output's internal key.
	Cosigners []*btcec.PublicKey

	// SweepLeaf is the operator's timeout leaf, committed as the asset
	// commitment's tapscript sibling.
	SweepLeaf txscript.TapLeaf

	// Digest scopes the batch output's deterministic OP_TRUE asset
	// script key. The key must be identical in the pre-funding
	// derivation and the commit, and the tree's root node spends it
	// with a caller-provided OP_TRUE witness, matching every other
	// tree output.
	Digest tapsdk.Hash

	// OutputIndex is the batch output's position in the anchor
	// transaction.
	OutputIndex uint32

	// OutputValueSat is the batch output's Bitcoin value.
	OutputValueSat int64
}

// BatchAnchorScript is the pre-funding derivation of the batch output: the
// composed script the funding wallet must keep byte-identical, and the
// binding material later phases sign and verify against.
type BatchAnchorScript struct {
	// PkScript is the composed batch output script.
	PkScript []byte

	// InternalKey is the untweaked cosigner aggregate.
	InternalKey *btcec.PublicKey

	// SigningTweak is the combined taproot tweak: the BIP-341 root over
	// the sweep leaf and the asset commitment root.
	SigningTweak []byte

	// AssetRoot is the batch output's Taproot Asset commitment root.
	AssetRoot tapsdk.Hash
}

// BatchAnchorCommit is the sealed commitment transition: the persistence
// material and a ready root source for materializing the tree beneath the
// still-unconfirmed batch output.
type BatchAnchorCommit struct {
	// AssetRef is the canonical string encoding of the committed asset.
	AssetRef string

	// OutputIndex is the batch output's position in the anchor
	// transaction.
	OutputIndex uint32

	// PackageBytes is the sealed transfer package.
	PackageBytes []byte

	// AnchorPSBT is the tapd-processed anchor PSBT. It carries tapd's
	// signature for the funding UTXO input; the caller signs its own
	// funding inputs and finalizes.
	AnchorPSBT []byte

	// Script echoes the validated batch output derivation.
	Script BatchAnchorScript

	// RootSource chains an asset tree's root node off the unconfirmed
	// commitment transition through a compact proof path.
	RootSource TreeRootAssetSource
}

// BatchAnchorCommitter derives and commits asset batch outputs against a
// caller-funded anchor transaction.
type BatchAnchorCommitter struct {
	driver customAnchorDriver
}

// NewBatchAnchorCommitter returns a committer over the given tap-sdk
// wallet.
func NewBatchAnchorCommitter(wallet *tapsdk.Wallet) (*BatchAnchorCommitter,
	error) {

	if wallet == nil {
		return nil, fmt.Errorf("tap-sdk wallet is required")
	}

	return &BatchAnchorCommitter{driver: &sdkDriver{wallet: wallet}}, nil
}

// DeriveScript computes the composed batch output script before the anchor
// transaction is funded. The template must already spend the funding UTXO's
// anchor outpoint and carry the batch output at its planned index; funding
// later adds inputs and change but must not touch the derived script.
func (c *BatchAnchorCommitter) DeriveScript(ctx context.Context,
	req *BatchAnchorRequest, template *psbt.Packet) (*BatchAnchorScript,
	error) {

	balanced, err := balanceTemplate(template)
	if err != nil {
		return nil, err
	}

	request, internalKey, err := c.buildRequest(req, balanced)
	if err != nil {
		return nil, err
	}

	previews, err := c.driver.Preview(ctx, request, req.Source.Verifier)
	if err != nil {
		return nil, fmt.Errorf("preview batch anchor: %w", err)
	}

	for _, preview := range previews {
		if preview.anchorOutputIndex != req.OutputIndex {
			continue
		}

		pkScript, err := composedScript(
			internalKey, preview.merkleRoot,
		)
		if err != nil {
			return nil, err
		}

		return &BatchAnchorScript{
			PkScript:     pkScript,
			InternalKey:  internalKey,
			SigningTweak: preview.merkleRoot[:],
			AssetRoot:    preview.assetRoot,
		}, nil
	}

	return nil, fmt.Errorf("preview misses batch output %d",
		req.OutputIndex)
}

// Commit seals the commitment transition against the final funded anchor
// transaction and verifies fail-closed that tapd reproduced the derived
// batch output byte-for-byte.
func (c *BatchAnchorCommitter) Commit(ctx context.Context,
	req *BatchAnchorRequest, funded *psbt.Packet,
	derived *BatchAnchorScript) (*BatchAnchorCommit, error) {

	if derived == nil || len(derived.PkScript) == 0 {
		return nil, fmt.Errorf("derived batch anchor script is " +
			"required")
	}
	finalTx := funded.UnsignedTx
	if int(req.OutputIndex) >= len(finalTx.TxOut) {
		return nil, fmt.Errorf("batch output %d exceeds anchor outputs",
			req.OutputIndex)
	}
	if !bytes.Equal(
		finalTx.TxOut[req.OutputIndex].PkScript, derived.PkScript,
	) {
		return nil, fmt.Errorf("funded anchor output %d does not "+
			"carry the derived batch script", req.OutputIndex)
	}

	request, internalKey, err := c.buildRequest(req, funded)
	if err != nil {
		return nil, err
	}

	committed, err := c.driver.Commit(ctx, request, req.Source.Verifier)
	if err != nil {
		return nil, fmt.Errorf("commit batch anchor: %w", err)
	}

	if len(committed.outputs) != 1 {
		return nil, fmt.Errorf("batch anchor committed %d "+
			"outputs, want 1", len(committed.outputs))
	}
	out := committed.outputs[0]
	if out.anchorOutputIndex != req.OutputIndex {
		return nil, fmt.Errorf("batch anchor committed output index "+
			"%d, want %d", out.anchorOutputIndex, req.OutputIndex)
	}
	if out.amount != req.Amount {
		return nil, fmt.Errorf("batch anchor committed amount "+
			"%d, want %d", out.amount, req.Amount)
	}
	if out.anchorValueSat != req.OutputValueSat {
		return nil, fmt.Errorf("batch anchor committed value "+
			"%d, want %d", out.anchorValueSat, req.OutputValueSat)
	}
	if !bytes.Equal(out.taprootMerkleRoot[:], derived.SigningTweak) {
		return nil, fmt.Errorf("batch anchor merkle root diverged " +
			"from the pre-funding derivation")
	}
	if out.taprootAssetRoot != derived.AssetRoot {
		return nil, fmt.Errorf("batch anchor asset root diverged " +
			"from the pre-funding derivation")
	}

	// The composed script must reproduce from the committed roots and
	// stay byte-identical to both the derivation and the funded output.
	pkScript, err := composedScript(internalKey, out.taprootMerkleRoot)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(pkScript, derived.PkScript) {
		return nil, fmt.Errorf("committed batch output script does " +
			"not reproduce the derived script")
	}

	// tapd must not have altered the funded transaction: the sealed
	// anchor is byte-identical to what the caller funded.
	committedPacket, err := psbtutil.Parse(committed.anchorPSBT)
	if err != nil {
		return nil, fmt.Errorf("parse committed anchor PSBT: %w", err)
	}
	if committedPacket.UnsignedTx.TxHash() != finalTx.TxHash() {
		return nil, fmt.Errorf("committed anchor transaction " +
			"diverged from the funded transaction")
	}

	rootSource, err := batchRootSource(req, out, finalTx, derived)
	if err != nil {
		return nil, err
	}

	return &BatchAnchorCommit{
		AssetRef:     req.AssetRef.String(),
		OutputIndex:  req.OutputIndex,
		PackageBytes: append([]byte(nil), committed.packageBytes...),
		AnchorPSBT:   append([]byte(nil), committed.anchorPSBT...),
		Script:       *derived,
		RootSource:   rootSource,
	}, nil
}

// buildRequest assembles the caller-funded custom-anchor request shared by
// derivation and commit.
func (c *BatchAnchorCommitter) buildRequest(req *BatchAnchorRequest,
	anchor *psbt.Packet) (*tapsdk.CustomAnchorRequest, *btcec.PublicKey,
	error) {

	anchorTx := anchor.UnsignedTx

	if req.Amount == 0 {
		return nil, nil, fmt.Errorf("batch asset amount is required")
	}
	if len(req.Source.ProofFile) == 0 {
		return nil, nil, fmt.Errorf("funding proof file is required")
	}
	if req.Source.AnchorInternalKey == nil {
		return nil, nil, fmt.Errorf("funding anchor internal key is " +
			"required")
	}
	if len(req.SweepLeaf.Script) == 0 {
		return nil, nil, fmt.Errorf("sweep leaf is required")
	}
	if req.Digest == (tapsdk.Hash{}) {
		return nil, nil, fmt.Errorf("batch script key digest is " +
			"required")
	}

	// The anchor transaction must spend the funding UTXO's anchor outpoint.
	spendsFunding := false
	for _, txIn := range anchorTx.TxIn {
		if txIn.PreviousOutPoint == req.Source.AnchorOutpoint {
			spendsFunding = true
			break
		}
	}
	if !spendsFunding {
		return nil, nil, fmt.Errorf("anchor transaction does not "+
			"spend the funding anchor %s",
			req.Source.AnchorOutpoint)
	}

	internalKey, err := tree.ComputeInternalKey(req.Cosigners)
	if err != nil {
		return nil, nil, fmt.Errorf("aggregate batch cosigners: %w",
			err)
	}

	anchorBytes, err := psbtutil.Serialize(anchor)
	if err != nil {
		return nil, nil, fmt.Errorf("serialize anchor PSBT: %w", err)
	}

	witnessPlan := tapsdk.CustomAssetWitnessPlan{
		Mode: witnessBackendSigner,
	}
	if len(req.Source.Witness) != 0 {
		witnessPlan = tapsdk.CustomAssetWitnessPlan{
			Mode:  witnessCallerProvided,
			Stack: cloneByteSlices(req.Source.Witness),
		}
	}

	anchorSigner, err := tapsdk.ParseXOnlyPubKey(
		schnorr.SerializePubKey(req.Source.AnchorInternalKey),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("funding anchor signer: %w", err)
	}

	request := &tapsdk.CustomAnchorRequest{
		Inputs: []tapsdk.CustomAssetInput{{
			ID:       "batch-anchor-input",
			AssetRef: req.AssetRef,
			Amount:   req.Amount,
			ProofFile: append(
				[]byte(nil), req.Source.ProofFile...,
			),
			Witness: witnessPlan,
		}},
		Outputs: []tapsdk.CustomAssetOutput{{
			ID:                "batch-anchor-output",
			AssetRef:          req.AssetRef,
			Amount:            req.Amount,
			AnchorOutputIndex: req.OutputIndex,
			AnchorValueSat:    uint64(req.OutputValueSat),
			Script: tapsdk.CustomAssetScriptPlan{
				Mode: tapsdk.CustomAssetScriptOPTrue,
				OPTrue: &tapsdk.CustomAssetOPTrueScriptPlan{
					InternalKey: tapsdk.KeyDescriptor{
						RawKeyBytes: deterministicKey(
							req.Digest,
							"batch-anchor",
						),
					},
				},
			},
			Anchor: anchorPlan(
				internalKey, []txscript.TapLeaf{req.SweepLeaf},
			),
		}},
		AnchorPSBT: anchorBytes,
		Funding: tapsdk.CustomAnchorFundingPlan{
			Mode: tapsdk.CustomAnchorFundingCallerFundedExact,
			CallerFundedExact: &tapsdk.
				CustomAnchorCallerFundedExact{},
		},
		PassiveAssets: tapsdk.CustomAnchorPassiveAssets{
			Policy: tapsdk.CustomAnchorPassiveReject,
		},
		LossPolicy: tapsdk.CustomAnchorLossPolicy{
			Mode: tapsdk.CustomAnchorLossReject,
		},
		SigningPlans: batchSigningPlans(
			anchorTx, req.Source.AnchorOutpoint, anchorSigner,
		),
	}

	return request, internalKey, nil
}

// batchSigningPlans classifies every anchor input: the funding UTXO input is a
// tapd key spend, every other input is caller-signed funding.
func batchSigningPlans(tx *wire.MsgTx, funding wire.OutPoint,
	anchorSigner tapsdk.XOnlyPubKey) []tapsdk.CustomAnchorInputSigningPlan {

	plans := make(
		[]tapsdk.CustomAnchorInputSigningPlan, 0, len(tx.TxIn),
	)
	for idx, txIn := range tx.TxIn {
		if txIn.PreviousOutPoint == funding {
			plans = append(
				plans, tapsdk.CustomAnchorInputSigningPlan{
					InputIndex: uint32(idx),
					KeyPath: &tapsdk.
						CustomAnchorKeyPathSigningPlan{
						Signer: anchorSigner,
					},
				},
			)

			continue
		}

		plans = append(plans, tapsdk.CustomAnchorInputSigningPlan{
			InputIndex: uint32(idx),
			CallerSigned: &tapsdk.
				CustomAnchorCallerSignedPlan{},
		})
	}

	return plans
}

// batchRootSource assembles the compact-path root source chaining a tree
// beneath the unconfirmed commitment transition.
func batchRootSource(req *BatchAnchorRequest, out commitOutput,
	finalTx *wire.MsgTx,
	derived *BatchAnchorScript) (TreeRootAssetSource, error) {

	path := &tapsdk.AssetProofPath{
		Version: tapsdk.AssetProofPathVersionV0,
		ConfirmedBaseProof: append(
			[]byte(nil), req.Source.ProofFile...,
		),
		Steps: []tapsdk.AssetProofPathStep{{
			TransitionProof: append(
				[]byte(nil), out.proofBlob...,
			),
		}},
	}

	expected := &expectedUnconfirmedAnchor{
		stepIndex:        0,
		previousOutpoint: sdkOutpoint(req.Source.AnchorOutpoint),
		anchorOutpoint:   out.anchorOutpoint,
		transaction:      serializeTx(finalTx),
	}

	return TreeRootAssetSource{
		proofPath: path,
		Witness:   cloneByteSlices(out.opTrueWitness),
		Verifier: &treePathVerifier{
			base: req.Source.Verifier,
			steps: []*expectedUnconfirmedAnchor{
				expected,
			},
		},
		expectedSteps: []*expectedUnconfirmedAnchor{
			expected,
		},
		SigningTweak:  append([]byte(nil), derived.SigningTweak...),
		BatchPkScript: append([]byte(nil), derived.PkScript...),
	}, nil
}

// composedScript derives the P2TR script of the internal key tweaked with
// the combined taproot root.
func composedScript(internalKey *btcec.PublicKey,
	merkleRoot tapsdk.Hash) ([]byte, error) {

	outputKey := txscript.ComputeTaprootOutputKey(
		internalKey, merkleRoot[:],
	)

	script, err := txscript.PayToTaprootScript(outputKey)
	if err != nil {
		return nil, fmt.Errorf("compose batch output script: %w", err)
	}

	return script, nil
}

// balanceTemplate appends a caller-signed funding stub covering the full
// output value, so the pre-funding template passes the backend's balance
// validation. The derived asset roots are independent of Bitcoin inputs
// (full-value transitions carry no split commitment), so the stub never
// influences the derivation and is absent from the funded transaction.
func balanceTemplate(template *psbt.Packet) (*psbt.Packet, error) {
	stubValue := int64(0)
	for _, out := range template.UnsignedTx.TxOut {
		stubValue += out.Value
	}

	tx := template.UnsignedTx.Copy()
	tx.AddTxIn(wire.NewTxIn(&fundingStubOutpoint, nil, nil))

	balanced, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, fmt.Errorf("balance anchor template: %w", err)
	}
	copy(balanced.Inputs, template.Inputs)

	stubScript, err := txscript.PayToTaprootScript(
		txscript.ComputeTaprootKeyNoScript(&arkscript.ARKNUMSKey),
	)
	if err != nil {
		return nil, fmt.Errorf("funding stub script: %w", err)
	}
	balanced.Inputs[len(balanced.Inputs)-1].WitnessUtxo = &wire.TxOut{
		Value:    stubValue,
		PkScript: stubScript,
	}

	return balanced, nil
}

// fundingStubOutpoint is the synthetic outpoint of the derivation-time
// funding stub. It never appears in a committed transaction.
var fundingStubOutpoint = wire.OutPoint{
	Hash: chainhash.HashH([]byte("wavelength/batch-anchor/funding-stub")),
}
