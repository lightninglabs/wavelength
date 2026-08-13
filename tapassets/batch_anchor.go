package tapassets

import (
	"bytes"
	"context"
	"errors"
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

// BatchAnchorSource identifies one confirmed asset funding UTXO a
// commitment transition spends: its proof, its amount, the Bitcoin
// outpoint anchoring it (which must be an input of the anchor
// transaction), and the internal key authorizing that input's spend.
type BatchAnchorSource struct {
	// ProofFile is the funding UTXO's complete confirmed proof file.
	ProofFile []byte

	// Amount is this funding UTXO's asset amount. The request's total
	// must equal the sum across all sources.
	Amount uint64

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

// Logical output identifiers naming the transition's outputs across
// derivation, commit, and the sealed package.
const (
	batchAnchorOutputID = "batch-anchor-output"
	batchAnchorChangeID = "batch-anchor-change"
)

// BatchAnchorChange returns a funding surplus to the operator's own tapd
// wallet: the batch output consumes the request's amount and the
// remainder re-anchors under wallet-owned keys on its own BIP-86 anchor
// output, becoming ordinary spendable inventory once the anchor
// confirms. Both keys are derived once and pinned on the request: the
// split commitment binds the change script key, so derivation and
// commit must see the same key to reproduce the same batch script.
type BatchAnchorChange struct {
	// Amount is the change output's asset amount.
	Amount uint64

	// OutputIndex is the change anchor output's position in the anchor
	// transaction.
	OutputIndex uint32

	// OutputValueSat is the change anchor output's Bitcoin value.
	OutputValueSat int64

	// ScriptKey is the tapd wallet script key that owns the change.
	ScriptKey tapsdk.ScriptKey

	// AnchorInternalKey is the tapd wallet internal key of the change
	// anchor output.
	AnchorInternalKey tapsdk.InternalKey
}

// DeriveBatchAnchorChange derives and pins the wallet keys owning a batch
// transition's asset change. The output index is assigned by the planner
// when the anchor transaction's output positions are final.
func DeriveBatchAnchorChange(ctx context.Context, wallet *tapsdk.Wallet,
	amount uint64, outputValueSat int64) (*BatchAnchorChange, error) {

	if wallet == nil {
		return nil, fmt.Errorf("tap-sdk wallet is required")
	}
	if amount == 0 || outputValueSat <= 0 {
		return nil, fmt.Errorf("change amount and value are required")
	}

	scriptKey, err := wallet.Client().DeriveScriptKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("derive change script key: %w", err)
	}
	internalKey, err := wallet.Client().DeriveInternalKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("derive change anchor internal key: %w",
			err)
	}

	return &BatchAnchorChange{
		Amount:            amount,
		OutputValueSat:    outputValueSat,
		ScriptKey:         *scriptKey,
		AnchorInternalKey: *internalKey,
	}, nil
}

// internalPubKey parses the change anchor internal key.
func (c *BatchAnchorChange) internalPubKey() (*btcec.PublicKey, error) {
	key, err := btcec.ParsePubKey(c.AnchorInternalKey.PubKey[:])
	if err != nil {
		return nil, fmt.Errorf("change anchor internal key: %w", err)
	}

	return key, nil
}

// BatchAnchorRequest describes one asset batch output on a caller-funded
// anchor transaction: the funding UTXOs move into a single output whose
// taproot key is the cosigner aggregate tweaked with the combined tapscript
// root. Without change the transition is full-value and produces no split
// commitment, so the derived roots are independent of output positions.
// With change the split commitment binds every asset output's anchor
// index, so both output positions must be final before derivation.
type BatchAnchorRequest struct {
	// AssetRef identifies the asset carried by the batch output.
	AssetRef tapsdk.AssetRef

	// Amount is the batch output's total asset amount. The funding
	// sources must carry exactly this amount plus any change.
	Amount uint64

	// Change optionally returns the funding surplus to the operator's
	// tapd wallet on its own anchor output.
	Change *BatchAnchorChange

	// Sources identify the funding UTXOs the transition spends. Every
	// source anchors one input of the anchor transaction.
	Sources []BatchAnchorSource

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

	// ChangePkScript is the composed change anchor output script, set
	// only when the request carries change.
	ChangePkScript []byte
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

	previews, err := c.driver.Preview(
		ctx, request, sourcesVerifier(req.Sources),
	)
	if err != nil {
		return nil, fmt.Errorf("preview batch anchor: %w", err)
	}

	var derived *BatchAnchorScript
	for _, preview := range previews {
		if preview.logicalOutputID != batchAnchorOutputID {
			continue
		}

		pkScript, err := composedScript(
			internalKey, preview.merkleRoot,
		)
		if err != nil {
			return nil, err
		}

		derived = &BatchAnchorScript{
			PkScript:     pkScript,
			InternalKey:  internalKey,
			SigningTweak: preview.merkleRoot[:],
			AssetRoot:    preview.assetRoot,
		}

		break
	}
	if derived == nil {
		return nil, fmt.Errorf("preview misses batch output %d",
			req.OutputIndex)
	}
	if req.Change == nil {
		return derived, nil
	}

	changeScript, err := changePreviewScript(req.Change, previews)
	if err != nil {
		return nil, err
	}
	derived.ChangePkScript = changeScript

	return derived, nil
}

// changePreviewScript composes the change anchor output script from its
// previewed roots.
func changePreviewScript(change *BatchAnchorChange,
	previews []commitmentPreview) ([]byte, error) {

	changeKey, err := change.internalPubKey()
	if err != nil {
		return nil, err
	}

	for _, preview := range previews {
		if preview.logicalOutputID != batchAnchorChangeID {
			continue
		}

		return composedScript(changeKey, preview.merkleRoot)
	}

	return nil, fmt.Errorf("preview misses change output %d",
		change.OutputIndex)
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
	if err := checkFundedChange(req.Change, derived, finalTx); err != nil {
		return nil, err
	}

	request, internalKey, err := c.buildRequest(req, funded)
	if err != nil {
		return nil, err
	}

	committed, err := c.driver.Commit(
		ctx, request, sourcesVerifier(req.Sources),
	)
	if err != nil {
		return nil, fmt.Errorf("commit batch anchor: %w", err)
	}

	wantOutputs := 1
	if req.Change != nil {
		wantOutputs = 2
	}
	if len(committed.outputs) != wantOutputs {
		return nil, fmt.Errorf("batch anchor committed %d "+
			"outputs, want %d", len(committed.outputs), wantOutputs)
	}
	out, err := committedOutput(committed, batchAnchorOutputID)
	if err != nil {
		return nil, err
	}
	if err := checkCommittedChange(
		req.Change, derived, committed,
	); err != nil {
		return nil, err
	}
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

// committedOutput locates one committed output by its logical identifier.
func committedOutput(committed *commitResult,
	logicalID string) (commitOutput, error) {

	for i := range committed.outputs {
		if committed.outputs[i].logicalOutputID == logicalID {
			return committed.outputs[i], nil
		}
	}

	return commitOutput{}, fmt.Errorf("batch anchor commit misses "+
		"output %q", logicalID)
}

// checkFundedChange verifies the funded transaction carries the derived
// change script at the change position. Nil change passes.
func checkFundedChange(change *BatchAnchorChange, derived *BatchAnchorScript,
	finalTx *wire.MsgTx) error {

	if change == nil {
		return nil
	}
	if len(derived.ChangePkScript) == 0 {
		return fmt.Errorf("derived change script is required")
	}
	if int(change.OutputIndex) >= len(finalTx.TxOut) {
		return fmt.Errorf("change output %d exceeds anchor outputs",
			change.OutputIndex)
	}
	if !bytes.Equal(
		finalTx.TxOut[change.OutputIndex].PkScript,
		derived.ChangePkScript,
	) {
		return fmt.Errorf("funded anchor output %d does not carry the "+
			"derived change script", change.OutputIndex)
	}

	return nil
}

// checkCommittedChange verifies fail-closed that the committed change
// output matches the request and reproduces the derived change script.
// Nil change passes.
func checkCommittedChange(change *BatchAnchorChange, derived *BatchAnchorScript,
	committed *commitResult) error {

	if change == nil {
		return nil
	}

	out, err := committedOutput(committed, batchAnchorChangeID)
	if err != nil {
		return err
	}
	if out.anchorOutputIndex != change.OutputIndex {
		return fmt.Errorf("committed change output index %d, want %d",
			out.anchorOutputIndex, change.OutputIndex)
	}
	if out.amount != change.Amount {
		return fmt.Errorf("committed change amount %d, want %d",
			out.amount, change.Amount)
	}
	if out.anchorValueSat != change.OutputValueSat {
		return fmt.Errorf("committed change value %d, want %d",
			out.anchorValueSat, change.OutputValueSat)
	}

	changeKey, err := change.internalPubKey()
	if err != nil {
		return err
	}
	pkScript, err := composedScript(changeKey, out.taprootMerkleRoot)
	if err != nil {
		return err
	}
	if !bytes.Equal(pkScript, derived.ChangePkScript) {
		return fmt.Errorf("committed change output script does not " +
			"reproduce the derived script")
	}

	return nil
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
	if len(req.Sources) == 0 {
		return nil, nil, fmt.Errorf("at least one funding source is " +
			"required")
	}
	var sourceTotal uint64
	for i := range req.Sources {
		source := &req.Sources[i]
		if len(source.ProofFile) == 0 {
			return nil, nil, fmt.Errorf("funding proof file %d is "+
				"required", i)
		}
		if source.AnchorInternalKey == nil {
			return nil, nil, fmt.Errorf("funding anchor internal "+
				"key %d is required", i)
		}
		if source.Amount == 0 {
			return nil, nil, fmt.Errorf("funding amount %d is "+
				"required", i)
		}
		sourceTotal += source.Amount

		// The anchor transaction must spend every funding UTXO's
		// anchor outpoint.
		spendsFunding := false
		for _, txIn := range anchorTx.TxIn {
			if txIn.PreviousOutPoint == source.AnchorOutpoint {
				spendsFunding = true
				break
			}
		}
		if !spendsFunding {
			return nil, nil, fmt.Errorf("anchor transaction does "+
				"not spend the funding anchor %s",
				source.AnchorOutpoint)
		}
	}
	wantTotal := req.Amount
	if req.Change != nil {
		change := req.Change
		if change.Amount == 0 || change.OutputValueSat <= 0 {
			return nil, nil, fmt.Errorf("change amount and value " +
				"are required")
		}
		if change.OutputIndex == req.OutputIndex {
			return nil, nil, fmt.Errorf("change and batch outputs "+
				"share anchor index %d", change.OutputIndex)
		}
		wantTotal += change.Amount
	}
	if sourceTotal != wantTotal {
		return nil, nil, fmt.Errorf("funding sources carry %d units, "+
			"the batch and change carry %d", sourceTotal, wantTotal)
	}
	if len(req.SweepLeaf.Script) == 0 {
		return nil, nil, fmt.Errorf("sweep leaf is required")
	}
	if req.Digest == (tapsdk.Hash{}) {
		return nil, nil, fmt.Errorf("batch script key digest is " +
			"required")
	}

	internalKey, err := tree.ComputeInternalKey(req.Cosigners)
	if err != nil {
		return nil, nil, fmt.Errorf("aggregate batch cosigners: %w",
			err)
	}

	// Hand tapd a copy with the funding input's caller metadata
	// cleared: the operator packet carries a synthetic key-spend
	// appearance for its funding wallet's weight estimator, but tapd
	// resolves its own input and fills the real derivation material —
	// which the wallet backing tapd later needs to sign the input.
	sanitized, err := psbt.NewFromUnsignedTx(anchor.UnsignedTx.Copy())
	if err != nil {
		return nil, nil, fmt.Errorf("clone anchor PSBT: %w", err)
	}
	copy(sanitized.Inputs, anchor.Inputs)
	copy(sanitized.Outputs, anchor.Outputs)
	for idx, txIn := range sanitized.UnsignedTx.TxIn {
		for i := range req.Sources {
			outpoint := req.Sources[i].AnchorOutpoint
			if txIn.PreviousOutPoint == outpoint {
				sanitized.Inputs[idx] = psbt.PInput{}
				break
			}
		}
	}

	anchorBytes, err := psbtutil.Serialize(sanitized)
	if err != nil {
		return nil, nil, fmt.Errorf("serialize anchor PSBT: %w", err)
	}

	inputs := make([]tapsdk.CustomAssetInput, 0, len(req.Sources))
	for i := range req.Sources {
		source := &req.Sources[i]
		witnessPlan := tapsdk.CustomAssetWitnessPlan{
			Mode: witnessBackendSigner,
		}
		if len(source.Witness) != 0 {
			witnessPlan = tapsdk.CustomAssetWitnessPlan{
				Mode:  witnessCallerProvided,
				Stack: cloneByteSlices(source.Witness),
			}
		}
		inputs = append(inputs, tapsdk.CustomAssetInput{
			ID:       fmt.Sprintf("batch-anchor-input-%d", i),
			AssetRef: req.AssetRef,
			Amount:   source.Amount,
			ProofFile: append(
				[]byte(nil), source.ProofFile...,
			),
			Witness: witnessPlan,
		})
	}

	plans, err := batchSigningPlans(anchorTx, req.Sources)
	if err != nil {
		return nil, nil, err
	}

	outputs := []tapsdk.CustomAssetOutput{{
		ID:                batchAnchorOutputID,
		AssetRef:          req.AssetRef,
		Amount:            req.Amount,
		AnchorOutputIndex: req.OutputIndex,
		AnchorValueSat:    uint64(req.OutputValueSat),
		Script: tapsdk.CustomAssetScriptPlan{
			Mode: tapsdk.CustomAssetScriptOPTrue,
			OPTrue: &tapsdk.CustomAssetOPTrueScriptPlan{
				InternalKey: tapsdk.KeyDescriptor{
					RawKeyBytes: deterministicKey(
						req.Digest, "batch-anchor",
					),
				},
			},
		},
		Anchor: anchorPlan(
			internalKey, []txscript.TapLeaf{req.SweepLeaf},
		),
	}}
	if req.Change != nil {
		outputs = append(outputs, tapsdk.CustomAssetOutput{
			ID:                batchAnchorChangeID,
			AssetRef:          req.AssetRef,
			Amount:            req.Change.Amount,
			AnchorOutputIndex: req.Change.OutputIndex,
			AnchorValueSat: uint64(
				req.Change.OutputValueSat,
			),
			// The pinned wallet script key rides as an external
			// key: the wallet script mode would derive a fresh
			// key on every build, and derivation and commit must
			// build identical transitions.
			Script: tapsdk.CustomAssetScriptPlan{
				Mode: scriptExternal,
				External: &tapsdk.
					CustomAssetExternalScriptPlan{
					ScriptKey: req.Change.ScriptKey,
				},
			},
			Anchor: tapsdk.CustomAnchorOutputPlan{
				InternalKey: req.Change.AnchorInternalKey,
			},
		})
	}

	request := &tapsdk.CustomAnchorRequest{
		Inputs:     inputs,
		Outputs:    outputs,
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
		SigningPlans: plans,
	}

	return request, internalKey, nil
}

// batchSigningPlans assigns per-input signing plans. A funding input
// whose source carries a caller-provided witness is caller-signed (a
// boarded funding input is a boarding input at the Bitcoin level, signed
// by its owner through the round's boarding-signature flow); a
// wallet-owned funding input is a tapd key spend under its source's
// anchor internal key; every other input is caller-signed by definition.
func batchSigningPlans(tx *wire.MsgTx,
	sources []BatchAnchorSource) ([]tapsdk.CustomAnchorInputSigningPlan,
	error) {

	type fundingPlan struct {
		callerSigned bool
		signer       tapsdk.XOnlyPubKey
	}
	funding := make(map[wire.OutPoint]fundingPlan, len(sources))
	for i := range sources {
		source := &sources[i]
		signer, err := tapsdk.ParseXOnlyPubKey(
			schnorr.SerializePubKey(source.AnchorInternalKey),
		)
		if err != nil {
			return nil, fmt.Errorf("funding anchor signer %d: %w",
				i, err)
		}
		funding[source.AnchorOutpoint] = fundingPlan{
			callerSigned: len(source.Witness) != 0,
			signer:       signer,
		}
	}

	plans := make(
		[]tapsdk.CustomAnchorInputSigningPlan, 0, len(tx.TxIn),
	)
	for idx, txIn := range tx.TxIn {
		plan, ok := funding[txIn.PreviousOutPoint]
		if ok && !plan.callerSigned {
			plans = append(
				plans, tapsdk.CustomAnchorInputSigningPlan{
					InputIndex: uint32(idx),
					KeyPath: &tapsdk.
						CustomAnchorKeyPathSigningPlan{
						Signer: plan.signer,
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

	return plans, nil
}

// sourcesVerifier verifies each confirmed funding proof against the
// source that claims it: a proof passes when any source's verifier
// accepts it, and every source's verifier pins its own tip claim, so a
// proof can only satisfy the source it actually belongs to.
func sourcesVerifier(
	sources []BatchAnchorSource) tapsdk.ConfirmedProofVerifier {

	verifiers := make([]tapsdk.ConfirmedProofVerifier, 0, len(sources))
	for i := range sources {
		if sources[i].Verifier != nil {
			verifiers = append(verifiers, sources[i].Verifier)
		}
	}

	return &multiSourceVerifier{verifiers: verifiers}
}

// multiSourceVerifier fans a proof out to the per-source verifiers.
type multiSourceVerifier struct {
	verifiers []tapsdk.ConfirmedProofVerifier
}

// VerifyConfirmedProof accepts a proof any per-source verifier accepts.
func (v *multiSourceVerifier) VerifyConfirmedProof(ctx context.Context,
	proofFile []byte) (*tapsdk.ConfirmedProofVerification, error) {

	if len(v.verifiers) == 0 {
		return nil, fmt.Errorf("no funding source verifiers")
	}

	var errs []error
	for _, verifier := range v.verifiers {
		verification, err := verifier.VerifyConfirmedProof(
			ctx, proofFile,
		)
		if err == nil {
			return verification, nil
		}
		errs = append(errs, err)
	}

	return nil, fmt.Errorf("no funding source accepts the proof: %w",
		errors.Join(errs...))
}

// batchRootSource assembles the compact-path root source chaining a tree
// beneath the unconfirmed commitment transition.
func batchRootSource(req *BatchAnchorRequest, out commitOutput,
	finalTx *wire.MsgTx,
	derived *BatchAnchorScript) (TreeRootAssetSource, error) {

	step := tapsdk.AssetProofPathStep{
		TransitionProof: append([]byte(nil), out.proofBlob...),
	}

	// The transition names one of its inputs as the lineage it
	// continues, and the backend — not the caller — chooses which. The
	// path's confirmed base must be that input's proof; the remaining
	// sources ride along as additional bases on a V1 path, bound to the
	// transition by the path verifier. A single source is unambiguously
	// that input, so only a batch of several has to be asked.
	baseIndex := 0
	previousOutpoint := sdkOutpoint(req.Sources[0].AnchorOutpoint)
	if len(req.Sources) > 1 {
		summary, err := step.Summary()
		if err != nil {
			return TreeRootAssetSource{}, fmt.Errorf("summarize "+
				"batch transition: %w", err)
		}
		previousOutpoint = summary.PreviousAnchorOutpoint

		baseIndex = -1
		for i := range req.Sources {
			outpoint := sdkOutpoint(req.Sources[i].AnchorOutpoint)
			if outpoint == previousOutpoint {
				baseIndex = i

				break
			}
		}
		if baseIndex < 0 {
			return TreeRootAssetSource{}, fmt.Errorf("batch "+
				"transition continues %v, which is not a "+
				"batched source", previousOutpoint)
		}
	}

	path := &tapsdk.AssetProofPath{
		Version: tapsdk.AssetProofPathVersionV0,
		ConfirmedBaseProof: append(
			[]byte(nil), req.Sources[baseIndex].ProofFile...,
		),
		Steps: []tapsdk.AssetProofPathStep{
			step,
		},
	}
	if len(req.Sources) > 1 {
		path.Version = tapsdk.AssetProofPathVersionV1
		for i := range req.Sources {
			if i == baseIndex {
				continue
			}
			path.AdditionalBaseProofs = append(
				path.AdditionalBaseProofs,
				append(
					[]byte(nil),
					req.Sources[i].ProofFile...,
				),
			)
		}
	}

	expected := &expectedUnconfirmedAnchor{
		stepIndex:        0,
		previousOutpoint: previousOutpoint,
		anchorOutpoint:   out.anchorOutpoint,
		transaction:      serializeTx(finalTx),
	}

	return TreeRootAssetSource{
		proofPath: path,
		Witness:   cloneByteSlices(out.opTrueWitness),
		Verifier: &treePathVerifier{
			base: sourcesVerifier(req.Sources),
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
