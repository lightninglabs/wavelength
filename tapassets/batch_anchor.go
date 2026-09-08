package tapassets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

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

// BatchAnchorSource describes one confirmed asset input.
type BatchAnchorSource struct {
	// ProofFile is the input's confirmed proof file.
	ProofFile []byte

	// Amount is the input's asset amount.
	Amount uint64

	// Witness is empty when tapd should sign the asset input.
	Witness [][]byte

	// Verifier verifies ProofFile.
	Verifier tapsdk.ConfirmedProofVerifier

	// AnchorOutpoint is the Bitcoin output that holds the asset.
	AnchorOutpoint wire.OutPoint

	// AnchorInternalKey authorizes the Bitcoin input.
	AnchorInternalKey *btcec.PublicKey
}

const (
	batchAnchorOutputID = "batch-anchor-output"
	batchAnchorChangeID = "batch-anchor-change"
)

// ErrReconciliationRequired reports that publication may have succeeded and
// its outcome must be checked before retrying.
var ErrReconciliationRequired = errors.New("asset transition reconciliation " +
	"required")

// BatchAnchorChange returns surplus assets to the tapd wallet.
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

// DeriveBatchAnchorChange derives the wallet keys for an asset change output.
func DeriveBatchAnchorChange(ctx context.Context, wallet *tapsdk.Wallet,
	amount uint64, outputValueSat int64) (*BatchAnchorChange, error) {

	if wallet == nil {
		return nil, fmt.Errorf("tap-sdk wallet is required")
	}
	if amount == 0 || outputValueSat <= 0 {
		return nil, fmt.Errorf("change amount and value are required")
	}

	keys, err := wallet.DeriveKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("derive change keys: %w", err)
	}

	return &BatchAnchorChange{
		Amount:            amount,
		OutputValueSat:    outputValueSat,
		ScriptKey:         keys.ScriptKey,
		AnchorInternalKey: keys.InternalKey,
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

// BatchAnchorRequest moves confirmed assets into one caller-funded output.
// Output indexes must be final before deriving a request with change.
type BatchAnchorRequest struct {
	// AssetRef identifies the asset carried by the batch output.
	AssetRef tapsdk.AssetRef

	// Amount is the batch output's total asset amount. The funding
	// sources must carry exactly this amount plus any change.
	Amount uint64

	// Change optionally returns the funding surplus to the operator's
	// tapd wallet on its own anchor output.
	Change *BatchAnchorChange

	// Sources are the confirmed asset inputs.
	Sources []BatchAnchorSource

	// Cosigners aggregate into the batch output's internal key.
	Cosigners []*btcec.PublicKey

	// SweepLeaf is the operator's timeout path.
	SweepLeaf txscript.TapLeaf

	// Digest scopes the deterministic asset script key.
	Digest tapsdk.Hash

	// OutputIndex is the batch output's position in the anchor
	// transaction.
	OutputIndex uint32

	// OutputValueSat is the batch output's Bitcoin value.
	OutputValueSat int64
}

// BatchAnchorScript contains the derived batch output script and its roots.
type BatchAnchorScript struct {
	// PkScript is the composed batch output script.
	PkScript []byte

	// InternalKey is the untweaked cosigner aggregate.
	InternalKey *btcec.PublicKey

	// SigningTweak commits to the sweep leaf and asset root.
	SigningTweak []byte

	// AssetRoot is the batch output's Taproot Asset commitment root.
	AssetRoot tapsdk.Hash

	// ChangePkScript is the composed change anchor output script, set
	// only when the request carries change.
	ChangePkScript []byte
}

// BatchAnchorCommit contains the sealed transition and its tree root input.
type BatchAnchorCommit struct {
	// AssetRef identifies the committed asset.
	AssetRef string

	// OutputIndex is the batch output's position in the anchor
	// transaction.
	OutputIndex uint32

	// PackageBytes is the sealed transfer package.
	PackageBytes []byte

	// AnchorPSBT contains any input signatures produced by tapd.
	AnchorPSBT []byte

	// Script echoes the validated batch output derivation.
	Script BatchAnchorScript

	// RootSource provides the asset inputs for tree materialization.
	RootSource TreeRootAssetSource
}

// BatchAnchorCommitter creates caller-funded asset batch outputs.
type BatchAnchorCommitter struct {
	driver          batchAnchorDriver
	store           Store
	encodeProofPath func(*tapsdk.AssetProofPath) ([]byte, error)
	mu              sync.Mutex
}

// BatchAnchorCommitterConfig configures a BatchAnchorCommitter.
type BatchAnchorCommitterConfig struct {
	Wallet *tapsdk.Wallet
	Store  Store
}

// NewBatchAnchorCommitter creates a durable batch anchor committer.
func NewBatchAnchorCommitter(cfg BatchAnchorCommitterConfig) (
	*BatchAnchorCommitter, error) {

	if cfg.Wallet == nil {
		return nil, fmt.Errorf("tap-sdk wallet is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("asset transition store is required")
	}

	return &BatchAnchorCommitter{
		driver: &sdkDriver{
			wallet: cfg.Wallet,
		},
		store: cfg.Store,
	}, nil
}

// DeriveScript computes the batch output script before Bitcoin funding.
func (c *BatchAnchorCommitter) DeriveScript(ctx context.Context,
	req *BatchAnchorRequest, template *psbt.Packet) (*BatchAnchorScript,
	error) {

	if c == nil || c.driver == nil {
		return nil, fmt.Errorf("batch anchor committer is required")
	}

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

	preview, err := previewedOutput(
		previews, batchAnchorOutputID, req.OutputIndex,
	)
	if err != nil {
		return nil, err
	}
	pkScript, err := composedScript(internalKey, preview.merkleRoot)
	if err != nil {
		return nil, err
	}
	derived := &BatchAnchorScript{
		PkScript:     pkScript,
		InternalKey:  internalKey,
		SigningTweak: append([]byte(nil), preview.merkleRoot[:]...),
		AssetRoot:    preview.assetRoot,
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
	previews []outputCommitmentPreview) ([]byte, error) {

	changeKey, err := change.internalPubKey()
	if err != nil {
		return nil, err
	}

	preview, err := previewedOutput(
		previews, batchAnchorChangeID, change.OutputIndex,
	)
	if err != nil {
		return nil, err
	}

	return composedScript(changeKey, preview.merkleRoot)
}

// previewedOutput verifies that every issuance assigned to a Bitcoin output
// produces the same commitment roots.
func previewedOutput(previews []outputCommitmentPreview, logicalID string,
	outputIndex uint32) (outputCommitmentPreview, error) {

	var result *outputCommitmentPreview
	for idx := range previews {
		preview := &previews[idx]
		if preview.logicalOutputID != logicalID {
			continue
		}
		if preview.anchorOutputIndex != outputIndex {
			return outputCommitmentPreview{}, fmt.Errorf("preview "+
				"%q uses output %d, want %d", logicalID,
				preview.anchorOutputIndex, outputIndex)
		}
		if result == nil {
			outputPreview := *preview
			result = &outputPreview

			continue
		}
		if preview.assetRoot != result.assetRoot ||
			preview.merkleRoot != result.merkleRoot {
			return outputCommitmentPreview{}, fmt.Errorf("preview "+
				"%q has inconsistent commitment roots",
				logicalID)
		}
	}
	if result == nil {
		return outputCommitmentPreview{}, fmt.Errorf("preview misses "+
			"output %q", logicalID)
	}

	return *result, nil
}

// Commit seals and validates the transition for a funded anchor transaction.
func (c *BatchAnchorCommitter) Commit(ctx context.Context,
	req *BatchAnchorRequest, funded *psbt.Packet,
	derived *BatchAnchorScript) (*BatchAnchorCommit, error) {

	if c == nil || c.driver == nil {
		return nil, fmt.Errorf("batch anchor committer is required")
	}
	if c.store == nil {
		return nil, fmt.Errorf("asset transition store is required")
	}
	if req == nil {
		return nil, fmt.Errorf("batch anchor request is required")
	}
	if funded == nil || funded.UnsignedTx == nil {
		return nil, fmt.Errorf("funded anchor PSBT is required")
	}

	// Keep the journal check and tapd mutation in one critical section.
	c.mu.Lock()
	defer c.mu.Unlock()

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

	journal := customAnchorCommitJournal{
		store:        c.store,
		driver:       c.driver,
		operation:    "batch anchor",
		digestDomain: "wavelength/asset-batch-request/v0",
	}
	journalKey := fmt.Sprintf("asset-batch/%s/%s/%d", req.Digest,
		req.AssetRef, req.OutputIndex)
	committed, err := journal.commitDurably(
		ctx, journalKey, request, sourcesVerifier(req.Sources),
	)
	if err != nil {
		return nil, fmt.Errorf("commit batch anchor: %w", err)
	}
	if len(committed.packageBytes) == 0 {
		return nil, fmt.Errorf("batch anchor commit misses sealed " +
			"package")
	}
	if committed.fundingMode !=
		tapsdk.CustomAnchorFundingCallerFundedExact {
		return nil, fmt.Errorf("unexpected funding mode %d",
			committed.fundingMode)
	}
	if err := validateBatchInputs(req, committed); err != nil {
		return nil, err
	}

	if err := validateCommittedOutputIDs(
		committed, req.Change != nil,
	); err != nil {
		return nil, err
	}
	batchOutputs, err := committedOutputs(
		committed, batchAnchorOutputID,
	)
	if err != nil {
		return nil, err
	}
	if err := checkCommittedChange(
		req.Change, req.AssetRef, finalTx, derived, committed,
	); err != nil {
		return nil, err
	}
	out := batchOutputs.outputs[0]
	if out.anchorOutputIndex != req.OutputIndex {
		return nil, fmt.Errorf("batch anchor committed output index "+
			"%d, want %d", out.anchorOutputIndex, req.OutputIndex)
	}
	if batchOutputs.amount != req.Amount {
		return nil, fmt.Errorf("batch anchor committed amount "+
			"%d, want %d", batchOutputs.amount, req.Amount)
	}
	if out.anchorValueSat != req.OutputValueSat {
		return nil, fmt.Errorf("batch anchor committed value "+
			"%d, want %d", out.anchorValueSat, req.OutputValueSat)
	}
	for idx := range batchOutputs.outputs {
		output := &batchOutputs.outputs[idx]
		if !output.assetRef.Equivalent(req.AssetRef) {
			return nil, fmt.Errorf("batch anchor output %d asset "+
				"ref mismatch", idx)
		}
		if output.scriptMode != tapsdk.CustomAssetScriptOPTrue {
			return nil, fmt.Errorf("batch anchor output %d script "+
				"mode %d is not OP_TRUE", idx,
				output.scriptMode)
		}
		if len(output.opTrueWitness) == 0 ||
			len(output.proofBlob) == 0 {
			return nil, fmt.Errorf("batch anchor output %d misses "+
				"witness or proof material", idx)
		}
	}
	if out.taprootMerkleRoot == (tapsdk.Hash{}) ||
		out.taprootAssetRoot == (tapsdk.Hash{}) {
		return nil, fmt.Errorf("batch anchor commit misses taproot " +
			"roots")
	}
	if !bytes.Equal(out.taprootMerkleRoot[:], derived.SigningTweak) {
		return nil, fmt.Errorf("batch anchor merkle root diverged " +
			"from the pre-funding derivation")
	}
	if out.taprootAssetRoot != derived.AssetRoot {
		return nil, fmt.Errorf("batch anchor asset root diverged " +
			"from the pre-funding derivation")
	}

	pkScript, err := composedScript(internalKey, out.taprootMerkleRoot)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(pkScript, derived.PkScript) {
		return nil, fmt.Errorf("committed batch output script does " +
			"not reproduce the derived script")
	}

	committedPacket, err := psbtutil.Parse(committed.anchorPSBT)
	if err != nil {
		return nil, fmt.Errorf("parse committed anchor PSBT: %w", err)
	}
	if committedPacket.UnsignedTx.TxHash() != finalTx.TxHash() {
		return nil, fmt.Errorf("committed anchor transaction " +
			"diverged from the funded transaction")
	}
	expectedOutpoint := sdkOutpoint(wire.OutPoint{
		Hash:  finalTx.TxHash(),
		Index: req.OutputIndex,
	})
	if out.anchorOutpoint != expectedOutpoint {
		return nil, fmt.Errorf("batch anchor committed outpoint " +
			"mismatch")
	}

	rootSource, err := batchRootSource(
		req, batchOutputs.outputs, committed.inputs, finalTx, derived,
		c.encodeProofPath,
	)
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

// Publish verifies a finalized anchor PSBT and records it in tapd.
func (c *BatchAnchorCommitter) Publish(ctx context.Context, packageBytes,
	finalPSBT []byte) error {

	if c == nil || c.driver == nil {
		return fmt.Errorf("batch anchor committer is required")
	}
	if len(packageBytes) == 0 || len(finalPSBT) == 0 {
		return fmt.Errorf("sealed package and final anchor PSBT are " +
			"required")
	}

	err := c.driver.Publish(ctx, packageBytes, finalPSBT)
	if err != nil {
		var attemptErr *tapsdk.CustomAnchorPublishAttemptError
		if errors.As(err, &attemptErr) && attemptErr.OutcomeUnknown {
			return errors.Join(
				ErrReconciliationRequired,
				fmt.Errorf("publish batch anchor transfer: %w",
					err),
			)
		}

		return fmt.Errorf("publish batch anchor transfer: %w", err)
	}

	return nil
}

// validateBatchInputs matches committed inputs to their requested sources.
func validateBatchInputs(req *BatchAnchorRequest,
	committed *commitResult) error {

	if len(committed.inputs) != len(req.Sources) {
		return fmt.Errorf("batch anchor committed %d inputs, want %d",
			len(committed.inputs), len(req.Sources))
	}

	byID := make(map[string]*commitInput, len(committed.inputs))
	for idx := range committed.inputs {
		input := &committed.inputs[idx]
		if _, ok := byID[input.logicalInputID]; ok {
			return fmt.Errorf("batch anchor repeats committed "+
				"input %q", input.logicalInputID)
		}
		byID[input.logicalInputID] = input
	}

	for idx := range req.Sources {
		source := &req.Sources[idx]
		id := fmt.Sprintf("batch-anchor-input-%d", idx)
		input, ok := byID[id]
		if !ok {
			return fmt.Errorf("batch anchor commit misses input %q",
				id)
		}
		if input.anchorOutpoint != sdkOutpoint(source.AnchorOutpoint) {
			return fmt.Errorf("batch anchor input %d outpoint "+
				"mismatch", idx)
		}
		if !input.assetRef.Equivalent(req.AssetRef) {
			return fmt.Errorf("batch anchor input %d asset ref "+
				"mismatch", idx)
		}
		if input.amount != source.Amount {
			return fmt.Errorf("batch anchor input %d amount "+
				"%d, want %d", idx, input.amount, source.Amount)
		}
		if input.proofSource.kind !=
			tapsdk.CustomAnchorProofSourceConfirmedFile ||
			!bytes.Equal(input.proofSource.blob, source.ProofFile) {
			return fmt.Errorf("batch anchor input %d proof source "+
				"mismatch", idx)
		}
	}

	return nil
}

type committedOutputSet struct {
	outputs []commitOutput
	amount  uint64
}

// validateCommittedOutputIDs rejects unexpected logical outputs.
func validateCommittedOutputIDs(committed *commitResult, hasChange bool) error {
	for idx := range committed.outputs {
		logicalID := committed.outputs[idx].logicalOutputID
		if logicalID == batchAnchorOutputID ||
			(hasChange && logicalID == batchAnchorChangeID) {

			continue
		}

		return fmt.Errorf("batch anchor committed unexpected output %q",
			logicalID)
	}

	return nil
}

// committedOutputs groups issuance outputs that share a Bitcoin output.
func committedOutputs(committed *commitResult,
	logicalID string) (*committedOutputSet, error) {

	result := &committedOutputSet{}
	seenIssuances := make(map[tapsdk.AssetID]struct{})
	for idx := range committed.outputs {
		output := committed.outputs[idx]
		if output.logicalOutputID != logicalID {
			continue
		}
		if _, ok := seenIssuances[output.issuanceID]; ok {
			return nil, fmt.Errorf("output %q repeats issuance %x",
				logicalID, output.issuanceID)
		}
		seenIssuances[output.issuanceID] = struct{}{}
		if output.amount > math.MaxUint64-result.amount {
			return nil, fmt.Errorf("output %q amount overflow",
				logicalID)
		}
		result.amount += output.amount

		if len(result.outputs) != 0 {
			first := result.outputs[0]
			if !sameCommittedAnchor(output, first) {
				return nil, fmt.Errorf("output %q has "+
					"inconsistent anchor data", logicalID)
			}
		}

		result.outputs = append(result.outputs, output)
	}
	if len(result.outputs) == 0 {
		return nil, fmt.Errorf("batch anchor commit misses output %q",
			logicalID)
	}

	return result, nil
}

// sameCommittedAnchor compares the shared fields of issuance outputs.
func sameCommittedAnchor(first, second commitOutput) bool {
	return first.anchorOutputIndex == second.anchorOutputIndex &&
		first.anchorOutpoint == second.anchorOutpoint &&
		first.anchorValueSat == second.anchorValueSat &&
		first.taprootAssetRoot == second.taprootAssetRoot &&
		first.taprootMerkleRoot == second.taprootMerkleRoot
}

// checkFundedChange verifies the optional change script.
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

// checkCommittedChange verifies the committed change allocation and script.
func checkCommittedChange(change *BatchAnchorChange, assetRef tapsdk.AssetRef,
	finalTx *wire.MsgTx, derived *BatchAnchorScript,
	committed *commitResult) error {

	if change == nil {
		return nil
	}

	changeOutputs, err := committedOutputs(committed, batchAnchorChangeID)
	if err != nil {
		return err
	}
	out := changeOutputs.outputs[0]
	if out.anchorOutputIndex != change.OutputIndex {
		return fmt.Errorf("committed change output index %d, want %d",
			out.anchorOutputIndex, change.OutputIndex)
	}
	if changeOutputs.amount != change.Amount {
		return fmt.Errorf("committed change amount %d, want %d",
			changeOutputs.amount, change.Amount)
	}
	if out.anchorValueSat != change.OutputValueSat {
		return fmt.Errorf("committed change value %d, want %d",
			out.anchorValueSat, change.OutputValueSat)
	}
	for idx := range changeOutputs.outputs {
		output := &changeOutputs.outputs[idx]
		if !output.assetRef.Equivalent(assetRef) {
			return fmt.Errorf("committed change output %d asset "+
				"ref mismatch", idx)
		}
		if output.scriptMode != scriptExternal {
			return fmt.Errorf("committed change output %d script "+
				"mode %d is not external", idx,
				output.scriptMode)
		}
		if len(output.proofBlob) == 0 {
			return fmt.Errorf("committed change output %d "+
				"misses proof", idx)
		}
	}
	if out.taprootMerkleRoot == (tapsdk.Hash{}) ||
		out.taprootAssetRoot == (tapsdk.Hash{}) {
		return fmt.Errorf("committed change misses taproot roots")
	}
	expectedOutpoint := sdkOutpoint(wire.OutPoint{
		Hash:  finalTx.TxHash(),
		Index: change.OutputIndex,
	})
	if out.anchorOutpoint != expectedOutpoint {
		return fmt.Errorf("committed change outpoint mismatch")
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

// buildRequest creates the SDK request used by derivation and commit.
func (c *BatchAnchorCommitter) buildRequest(req *BatchAnchorRequest,
	anchor *psbt.Packet) (*tapsdk.CustomAnchorRequest, *btcec.PublicKey,
	error) {

	if err := validateBatchAnchorRequest(req, anchor); err != nil {
		return nil, nil, err
	}
	anchorTx := anchor.UnsignedTx

	cosigners := tree.UniqueCosigners(req.Cosigners)
	internalKey, err := tree.ComputeInternalKey(cosigners)
	if err != nil {
		return nil, nil, fmt.Errorf("aggregate batch cosigners: %w",
			err)
	}

	// tapd replaces the placeholder metadata for inputs it signs.
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
			// Reuse the key derived before the anchor was funded.
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

func validateBatchAnchorRequest(req *BatchAnchorRequest,
	anchor *psbt.Packet) error {

	if req == nil {
		return fmt.Errorf("batch anchor request is required")
	}
	if anchor == nil || anchor.UnsignedTx == nil {
		return fmt.Errorf("anchor PSBT is required")
	}
	if err := req.AssetRef.Validate(); err != nil {
		return fmt.Errorf("asset ref: %w", err)
	}
	if req.Amount == 0 {
		return fmt.Errorf("batch asset amount is required")
	}
	if len(req.Sources) == 0 {
		return fmt.Errorf("at least one funding source is required")
	}
	for idx, cosigner := range req.Cosigners {
		if cosigner == nil {
			return fmt.Errorf("batch cosigner %d is required", idx)
		}
	}

	sourceTotal, err := validateBatchSources(req.Sources, anchor.UnsignedTx)
	if err != nil {
		return err
	}
	wantTotal := req.Amount
	if req.Change != nil {
		change := req.Change
		if change.Amount == 0 || change.OutputValueSat <= 0 {
			return fmt.Errorf("change amount and value are " +
				"required")
		}
		if change.OutputIndex == req.OutputIndex {
			return fmt.Errorf("change and batch outputs share "+
				"anchor index %d", change.OutputIndex)
		}
		if change.Amount > math.MaxUint64-wantTotal {
			return fmt.Errorf("batch and change amount overflow")
		}
		wantTotal += change.Amount
	}
	if sourceTotal != wantTotal {
		return fmt.Errorf("funding sources carry %d units, the batch "+
			"and change carry %d", sourceTotal, wantTotal)
	}
	if len(req.SweepLeaf.Script) == 0 {
		return fmt.Errorf("sweep leaf is required")
	}
	if req.OutputValueSat <= 0 {
		return fmt.Errorf("batch output value is required")
	}
	if int(req.OutputIndex) >= len(anchor.UnsignedTx.TxOut) {
		return fmt.Errorf("batch output %d exceeds anchor outputs",
			req.OutputIndex)
	}
	if req.Digest == (tapsdk.Hash{}) {
		return fmt.Errorf("batch script key digest is required")
	}

	return nil
}

func validateBatchSources(sources []BatchAnchorSource,
	anchorTx *wire.MsgTx) (uint64, error) {

	var sourceTotal uint64
	seenProofs := make(map[string]struct{}, len(sources))
	sourcesByOutpoint := make(
		map[wire.OutPoint]*BatchAnchorSource, len(sources),
	)
	for i := range sources {
		source := &sources[i]
		if len(source.ProofFile) == 0 {
			return 0, fmt.Errorf("funding proof file %d is "+
				"required", i)
		}
		if source.AnchorInternalKey == nil {
			return 0, fmt.Errorf("funding anchor internal key %d "+
				"is required", i)
		}
		if source.Amount == 0 {
			return 0, fmt.Errorf("funding amount %d is required", i)
		}
		if source.Verifier == nil {
			return 0, fmt.Errorf("funding proof verifier %d is "+
				"required", i)
		}
		proofKey := string(source.ProofFile)
		if _, ok := seenProofs[proofKey]; ok {
			return 0, fmt.Errorf("funding proof file %d is "+
				"repeated", i)
		}
		seenProofs[proofKey] = struct{}{}
		previous := sourcesByOutpoint[source.AnchorOutpoint]
		if previous != nil {
			sameKey := previous.AnchorInternalKey.IsEqual(
				source.AnchorInternalKey,
			)
			sameSigner := (len(previous.Witness) == 0) ==
				(len(source.Witness) == 0)
			if !sameKey || !sameSigner {
				return 0, fmt.Errorf("funding sources at %s "+
					"use different anchor signing plans",
					source.AnchorOutpoint)
			}
		} else {
			sourcesByOutpoint[source.AnchorOutpoint] = source
		}
		if source.Amount > math.MaxUint64-sourceTotal {
			return 0, fmt.Errorf("funding source amount overflow")
		}
		sourceTotal += source.Amount

	}

	for outpoint := range sourcesByOutpoint {
		fundingSpends := 0
		for _, txIn := range anchorTx.TxIn {
			if txIn.PreviousOutPoint == outpoint {
				fundingSpends++
			}
		}
		if fundingSpends == 0 {
			return 0, fmt.Errorf("anchor transaction does not "+
				"spend the funding anchor %s", outpoint)
		}
		if fundingSpends != 1 {
			return 0, fmt.Errorf("anchor transaction spends the "+
				"funding anchor %s %d times", outpoint,
				fundingSpends)
		}
	}

	return sourceTotal, nil
}

// batchSigningPlans assigns tapd-owned inputs and caller-owned inputs.
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

// sourcesVerifier combines the verifiers for all requested sources.
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

// batchRootSource builds one proof path for each issuance held by the batch
// output.
func batchRootSource(req *BatchAnchorRequest, outputs []commitOutput,
	inputs []commitInput, finalTx *wire.MsgTx, derived *BatchAnchorScript,
	encodeProofPath func(*tapsdk.AssetProofPath) ([]byte, error)) (
	TreeRootAssetSource, error) {

	root := TreeRootAssetSource{
		Inputs:        make([]TreeRootAssetInput, 0, len(outputs)),
		SigningTweak:  append([]byte(nil), derived.SigningTweak...),
		BatchPkScript: append([]byte(nil), derived.PkScript...),
	}
	anchorOutpoint := wire.OutPoint{
		Hash:  finalTx.TxHash(),
		Index: req.OutputIndex,
	}
	anchorTx := serializeTx(finalTx)
	for idx := range outputs {
		output := outputs[idx]
		rootInput, err := batchRootInput(
			req, output, inputs, anchorOutpoint, anchorTx,
			encodeProofPath,
		)
		if err != nil {
			return TreeRootAssetSource{}, fmt.Errorf("batch "+
				"output issuance %x: %w", output.issuanceID,
				err)
		}
		root.Inputs = append(root.Inputs, rootInput)
	}

	return root, nil
}

// batchRootInput joins one issuance's confirmed inputs to its batch
// transition.
func batchRootInput(req *BatchAnchorRequest, output commitOutput,
	inputs []commitInput, anchorOutpoint wire.OutPoint, anchorTx []byte,
	encodeProofPath func(*tapsdk.AssetProofPath) ([]byte, error)) (
	TreeRootAssetInput, error) {

	step := tapsdk.AssetProofPathStep{
		TransitionProof: append([]byte(nil), output.proofBlob...),
	}

	packetInputs := make([]commitInput, 0, len(inputs))
	for idx := range inputs {
		input := inputs[idx]
		if input.packetRole == output.packetRole &&
			input.packetIndex == output.packetIndex {

			packetInputs = append(packetInputs, input)
		}
	}
	if len(packetInputs) == 0 {
		return TreeRootAssetInput{}, fmt.Errorf("transition has no " +
			"inputs")
	}
	sort.Slice(packetInputs, func(i, j int) bool {
		return packetInputs[i].virtualInputIndex <
			packetInputs[j].virtualInputIndex
	})

	for idx := range packetInputs {
		input := packetInputs[idx]
		if input.virtualInputIndex != uint32(idx) {
			return TreeRootAssetInput{}, fmt.Errorf("packet " +
				"input indexes are not contiguous")
		}
		if input.issuanceID != output.issuanceID {
			return TreeRootAssetInput{}, fmt.Errorf("packet input "+
				"%d has a different issuance", idx)
		}
	}
	base := packetInputs[0]
	previousOutpoint := base.anchorOutpoint
	if len(packetInputs) > 1 {
		summary, err := step.Summary()
		if err != nil {
			return TreeRootAssetInput{}, fmt.Errorf("summarize "+
				"transition: %w", err)
		}
		if summary.IssuanceID != output.issuanceID ||
			!summary.AssetRef.Equivalent(output.assetRef) ||
			summary.Amount != output.amount ||
			summary.AnchorOutpoint != output.anchorOutpoint ||
			summary.AnchorValueSat != output.anchorValueSat {
			return TreeRootAssetInput{}, fmt.Errorf("transition " +
				"summary does not match committed output")
		}
		previousOutpoint = summary.PreviousAnchorOutpoint
		if base.anchorOutpoint != previousOutpoint {
			return TreeRootAssetInput{}, fmt.Errorf("transition " +
				"base does not match packet input zero")
		}
	}

	path := &tapsdk.AssetProofPath{
		ConfirmedBaseProof: append(
			[]byte(nil), base.proofSource.blob...,
		),
		Steps: []tapsdk.AssetProofPathStep{
			step,
		},
	}
	previousOutpoints := []tapsdk.Outpoint{base.anchorOutpoint}
	if len(packetInputs) > 1 {
		for idx := 1; idx < len(packetInputs); idx++ {
			input := packetInputs[idx]
			coPath := &tapsdk.AssetProofPath{
				ConfirmedBaseProof: append(
					[]byte(nil), input.proofSource.blob...,
				),
			}
			path.Steps[0].CoInputPaths = append(
				path.Steps[0].CoInputPaths, coPath,
			)
			previousOutpoints = append(
				previousOutpoints, input.anchorOutpoint,
			)
		}
	}
	if encodeProofPath == nil {
		encodeProofPath = func(path *tapsdk.AssetProofPath) ([]byte,
			error) {

			return path.MarshalBinary()
		}
	}
	encodedPath, err := encodeProofPath(path)
	if err != nil {
		return TreeRootAssetInput{}, fmt.Errorf("encode proof path: %w",
			err)
	}

	expected := &expectedUnconfirmedAnchor{
		previousOutpoint:  previousOutpoint,
		previousOutpoints: previousOutpoints,
		anchorOutpoint:    output.anchorOutpoint,
		transaction:       append([]byte(nil), anchorTx...),
	}

	return TreeRootAssetInput{
		ProofPath:      encodedPath,
		Amount:         output.amount,
		AnchorOutpoint: anchorOutpoint,
		Witness:        cloneByteSlices(output.opTrueWitness),
		Verifier: &treePathVerifier{
			base:      sourcesVerifier(req.Sources),
			baseDepth: 0,
			steps: []*expectedUnconfirmedAnchor{
				expected,
			},
		},
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

// balanceTemplate funds a copy of the pre-funding template for SDK preview.
func balanceTemplate(template *psbt.Packet) (*psbt.Packet, error) {
	if template == nil || template.UnsignedTx == nil {
		return nil, fmt.Errorf("anchor template PSBT is required")
	}

	stubValue := int64(0)
	for _, out := range template.UnsignedTx.TxOut {
		if out.Value < 0 || out.Value > math.MaxInt64-stubValue {
			return nil, fmt.Errorf("anchor template output value " +
				"overflow")
		}
		stubValue += out.Value
	}

	tx := template.UnsignedTx.Copy()
	tx.AddTxIn(wire.NewTxIn(&fundingStubOutpoint, nil, nil))

	balanced, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, fmt.Errorf("balance anchor template: %w", err)
	}
	copy(balanced.Inputs, template.Inputs)
	copy(balanced.Outputs, template.Outputs)

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
