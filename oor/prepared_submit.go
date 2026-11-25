package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
)

// PreparedSubmitPackage is an OOR Bitcoin graph whose Taproot Asset
// commitments were already inserted by an external tap-sdk orchestration
// boundary (such as tapassets.BuildOORTransfer). The deterministic FSM never
// calls the asset engine; it validates and persists this immutable handoff
// before requesting any Bitcoin signature.
type PreparedSubmitPackage struct {
	// ArkPSBT is the committed checkpoint-to-recipient anchor transaction.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are the committed input-to-checkpoint anchor
	// transactions, in transfer-input order.
	CheckpointPSBTs []*psbt.Packet

	// CheckpointAssetRoots holds the optional Taproot Asset commitment
	// root anchored in each checkpoint output, in transfer-input order. A
	// non-nil slot corresponds to an asset-bearing checkpoint whose Ark
	// spend control block is composed via ComposeWithSiblingRoot; a nil
	// slot corresponds to a plain Bitcoin checkpoint (such as a leased
	// carrier float).
	CheckpointAssetRoots []*chainhash.Hash

	// TaprootAssetTransfer carries the sealed tap-sdk package for every
	// checkpoint transition and the final Ark transition.
	TaprootAssetTransfer *oortx.TaprootAssetTransfer
}

// clone deep-copies the prepared PSBTs, checkpoint asset roots, and sealed
// asset transfer container so the FSM owns an isolated copy and never mutates
// or aliases caller-owned packets.
func (p *PreparedSubmitPackage) clone() (*PreparedSubmitPackage, error) {
	if p == nil {
		return nil, fmt.Errorf("prepared submit package must be " +
			"provided")
	}

	arkRaw, err := psbtutil.Serialize(p.ArkPSBT)
	if err != nil {
		return nil, fmt.Errorf("clone prepared ark psbt: %w", err)
	}
	arkClone, err := psbtutil.Parse(arkRaw)
	if err != nil {
		return nil, fmt.Errorf("parse cloned prepared ark psbt: %w",
			err)
	}

	cpClones := make([]*psbt.Packet, len(p.CheckpointPSBTs))
	for i, cp := range p.CheckpointPSBTs {
		cpRaw, err := psbtutil.Serialize(cp)
		if err != nil {
			return nil, fmt.Errorf("clone prepared checkpoint "+
				"%d: %w", i, err)
		}
		cpClone, err := psbtutil.Parse(cpRaw)
		if err != nil {
			return nil, fmt.Errorf("parse cloned prepared "+
				"checkpoint %d: %w", i, err)
		}
		cpClones[i] = cpClone
	}

	var rootClones []*chainhash.Hash
	if len(p.CheckpointAssetRoots) > 0 {
		rootClones = make(
			[]*chainhash.Hash, len(p.CheckpointAssetRoots),
		)
		for i, root := range p.CheckpointAssetRoots {
			if root != nil {
				copied := *root
				rootClones[i] = &copied
			}
		}
	}

	return &PreparedSubmitPackage{
		ArkPSBT:              arkClone,
		CheckpointPSBTs:      cpClones,
		CheckpointAssetRoots: rootClones,
		TaprootAssetTransfer: p.TaprootAssetTransfer.Clone(),
	}, nil
}

// AttachTapLeafScripts derives and attaches the owner-leaf TaprootLeafScript
// (composed with the checkpoint asset root for asset checkpoints, plain for
// non-asset checkpoints) onto every Ark PSBT input, verifying that each
// control block commits to the corresponding checkpoint output's pkScript.
func (p *PreparedSubmitPackage) AttachTapLeafScripts(
	inputs []TransferInput) error {

	if p == nil {
		return fmt.Errorf("prepared submit package must be provided")
	}
	if len(inputs) == 0 || len(inputs) != len(p.CheckpointPSBTs) {
		return fmt.Errorf("prepared checkpoint count does not match " +
			"inputs")
	}
	if len(p.CheckpointAssetRoots) != len(p.CheckpointPSBTs) {
		return fmt.Errorf("prepared checkpoint asset root count %d "+
			"does not match checkpoint count %d",
			len(p.CheckpointAssetRoots), len(p.CheckpointPSBTs))
	}
	if _, err := oortx.ValidateSubmitPackage(
		p.ArkPSBT, p.CheckpointPSBTs,
	); err != nil {
		return err
	}

	type cpEntry struct {
		inputIdx   int
		checkpoint *psbt.Packet
		assetRoot  *chainhash.Hash
		ownerLeaf  []byte
	}
	byTxid := make(map[chainhash.Hash]cpEntry, len(inputs))
	seenOutpoints := make(map[wire.OutPoint]struct{}, len(inputs))
	for i := range inputs {
		if err := inputs[i].Validate(); err != nil {
			return fmt.Errorf("prepared input %d: %w", i, err)
		}
		if _, dup := seenOutpoints[inputs[i].VTXO.Outpoint]; dup {
			return fmt.Errorf("duplicate prepared input "+
				"outpoint %s", inputs[i].VTXO.Outpoint)
		}
		seenOutpoints[inputs[i].VTXO.Outpoint] = struct{}{}

		cp := p.CheckpointPSBTs[i]
		if len(cp.UnsignedTx.TxIn) != 1 ||
			cp.UnsignedTx.TxIn[0].PreviousOutPoint !=
				inputs[i].VTXO.Outpoint {
			return fmt.Errorf("prepared checkpoint %d input "+
				"mismatch", i)
		}

		hasAssetInput := inputs[i].VTXO.TaprootAssetRoot != nil
		hasCheckpointRoot := p.CheckpointAssetRoots[i] != nil
		if hasAssetInput != hasCheckpointRoot {
			return fmt.Errorf("prepared input %d asset root and "+
				"checkpoint asset root presence mismatch", i)
		}

		byTxid[cp.UnsignedTx.TxHash()] = cpEntry{
			inputIdx:   i,
			checkpoint: cp,
			assetRoot:  p.CheckpointAssetRoots[i],
			ownerLeaf:  inputs[i].OwnerLeafScript,
		}
	}

	for i := range p.ArkPSBT.UnsignedTx.TxIn {
		prevOut := p.ArkPSBT.UnsignedTx.TxIn[i].PreviousOutPoint
		entry, ok := byTxid[prevOut.Hash]
		if !ok {
			return fmt.Errorf("missing prepared checkpoint for "+
				"ark input %d", i)
		}

		tapTree := entry.checkpoint.Outputs[0].TaprootTapTree
		var (
			leaf *psbt.TaprootTapLeafScript
			err  error
		)
		if entry.assetRoot != nil {
			leaf, err = oortx.BuildComposedTaprootTapLeafScript(
				tapTree, entry.ownerLeaf, *entry.assetRoot,
			)
		} else {
			leaf, err = oortx.BuildTaprootTapLeafScript(
				tapTree, entry.ownerLeaf,
			)
		}
		if err != nil {
			return fmt.Errorf("ark input %d tapleaf: %w", i, err)
		}

		spendPath := &arkscript.SpendPath{
			SpendInfo: &arkscript.SpendInfo{
				WitnessScript: leaf.Script,
				ControlBlock:  leaf.ControlBlock,
			},
		}
		cpScript := entry.checkpoint.UnsignedTx.TxOut[0].PkScript
		if err := spendPath.VerifyBindsToPkScript(
			cpScript,
		); err != nil {
			return fmt.Errorf("ark input %d tapleaf does not bind "+
				"to checkpoint %d pkScript: %w", i,
				entry.inputIdx, err)
		}

		addTaprootLeafScript(&p.ArkPSBT.Inputs[i], leaf)
	}

	return nil
}

// Validate binds the prepared Bitcoin graph and sealed asset packages to the
// session checkpoint policy, caller transfer inputs, and canonical recipient
// metadata, enforcing duplicate-input rejection, checkpoint policy and amount
// binding, and fee-less value conservation.
func (p *PreparedSubmitPackage) Validate(policy arkscript.CheckpointPolicy,
	inputs []TransferInput, recipients []oortx.RecipientOutput) error {

	if p == nil {
		return fmt.Errorf("prepared submit package must be provided")
	}
	if len(inputs) == 0 || len(inputs) != len(p.CheckpointPSBTs) {
		return fmt.Errorf("prepared checkpoint count does not match " +
			"inputs")
	}
	if len(recipients) == 0 {
		return fmt.Errorf("prepared recipients must be provided")
	}
	if p.TaprootAssetTransfer == nil {
		return fmt.Errorf("prepared taproot asset transfer is required")
	}
	if err := p.TaprootAssetTransfer.Validate(
		len(p.CheckpointPSBTs),
	); err != nil {
		return err
	}
	if _, err := oortx.ValidateSubmitPackage(
		p.ArkPSBT, p.CheckpointPSBTs,
	); err != nil {
		return err
	}
	if len(p.CheckpointAssetRoots) != len(p.CheckpointPSBTs) {
		return fmt.Errorf("prepared checkpoint asset root count %d "+
			"does not match checkpoint count %d",
			len(p.CheckpointAssetRoots), len(p.CheckpointPSBTs))
	}

	ownerLeafByCPTxid, inputTotal, err := p.validatePreparedCheckpoints(
		policy, inputs,
	)
	if err != nil {
		return err
	}

	arkInputTotal, err := p.validatePreparedArkInputs(ownerLeafByCPTxid)
	if err != nil {
		return err
	}

	return p.validatePreparedRecipients(
		recipients, inputTotal, arkInputTotal,
	)
}

// validatePreparedCheckpoints validates input uniqueness, asset slot and root
// alignment, checkpoint output values, and session checkpoint policy scripts.
func (p *PreparedSubmitPackage) validatePreparedCheckpoints(
	policy arkscript.CheckpointPolicy, inputs []TransferInput) (
	map[chainhash.Hash][]byte, btcutil.Amount, error) {

	ownerLeafByCPTxid := make(
		map[chainhash.Hash][]byte, len(p.CheckpointPSBTs),
	)
	seenOutpoints := make(map[wire.OutPoint]struct{}, len(inputs))
	var inputTotal btcutil.Amount
	for i := range inputs {
		if err := inputs[i].Validate(); err != nil {
			return nil, 0, fmt.Errorf("prepared input %d: %w", i,
				err)
		}
		if _, dup := seenOutpoints[inputs[i].VTXO.Outpoint]; dup {
			return nil, 0, fmt.Errorf("duplicate prepared input "+
				"outpoint %s", inputs[i].VTXO.Outpoint)
		}
		seenOutpoints[inputs[i].VTXO.Outpoint] = struct{}{}

		hasAssetRoot := inputs[i].VTXO.TaprootAssetRoot != nil
		hasAssetPackage := len(
			p.TaprootAssetTransfer.CheckpointPackages[i],
		) != 0
		if hasAssetRoot != hasAssetPackage {
			return nil, 0, fmt.Errorf("prepared input %d asset "+
				"root and checkpoint package presence mismatch",
				i)
		}
		if hasAssetRoot != (p.CheckpointAssetRoots[i] != nil) {
			return nil, 0, fmt.Errorf("prepared input %d asset "+
				"root and checkpoint asset root presence "+
				"mismatch", i)
		}
		if p.CheckpointAssetRoots[i] != nil &&
			*p.CheckpointAssetRoots[i] == (chainhash.Hash{}) {
			return nil, 0, fmt.Errorf("prepared checkpoint %d "+
				"asset root must not be zero", i)
		}

		checkpoint := p.CheckpointPSBTs[i]
		if len(checkpoint.UnsignedTx.TxIn) != 1 {
			return nil, 0, fmt.Errorf("prepared checkpoint %d "+
				"must have one input", i)
		}
		if checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint !=
			inputs[i].VTXO.Outpoint {
			return nil, 0, fmt.Errorf("prepared checkpoint %d "+
				"input mismatch", i)
		}
		if checkpoint.UnsignedTx.TxOut[0].Value !=
			int64(inputs[i].VTXO.Amount) {
			return nil, 0, fmt.Errorf("prepared checkpoint %d "+
				"output value %d does not match input "+
				"amount %d", i,
				checkpoint.UnsignedTx.TxOut[0].Value,
				inputs[i].VTXO.Amount)
		}

		wantCPPkScript, err := expectedCheckpointPkScript(
			policy, inputs[i].OwnerLeafScript,
			p.CheckpointAssetRoots[i],
		)
		if err != nil {
			return nil, 0, fmt.Errorf("prepared checkpoint %d "+
				"policy: %w", i, err)
		}
		if !bytes.Equal(
			checkpoint.UnsignedTx.TxOut[0].PkScript, wantCPPkScript,
		) {
			return nil, 0, fmt.Errorf("prepared checkpoint %d "+
				"pkScript does not match session "+
				"checkpoint policy", i)
		}

		if inputs[i].VTXO.Amount > btcutil.MaxSatoshi-inputTotal {
			return nil, 0, fmt.Errorf("prepared input amount sum " +
				"overflows")
		}
		inputTotal += inputs[i].VTXO.Amount

		ownerLeafByCPTxid[checkpoint.UnsignedTx.TxHash()] =
			inputs[i].OwnerLeafScript
	}

	return ownerLeafByCPTxid, inputTotal, nil
}

// validatePreparedArkInputs verifies every Ark input carries a valid owner-leaf
// control block bound to its checkpoint output pkScript and returns the summed
// Ark input value.
func (p *PreparedSubmitPackage) validatePreparedArkInputs(
	ownerLeafByCPTxid map[chainhash.Hash][]byte) (btcutil.Amount, error) {

	var arkInputTotal btcutil.Amount
	for i := range p.ArkPSBT.UnsignedTx.TxIn {
		prevOut := p.ArkPSBT.UnsignedTx.TxIn[i].PreviousOutPoint
		ownerLeaf, ok := ownerLeafByCPTxid[prevOut.Hash]
		if !ok {
			return 0, fmt.Errorf("prepared ark input %d unknown "+
				"checkpoint %s", i, prevOut.Hash)
		}

		leaf, err := findTapLeafByScript(
			&p.ArkPSBT.Inputs[i], ownerLeaf,
		)
		if err != nil {
			return 0, fmt.Errorf("prepared ark input %d: %w", i,
				err)
		}
		if leaf.LeafVersion != txscript.BaseLeafVersion {
			return 0, fmt.Errorf("prepared ark input %d "+
				"unexpected leaf version %d", i,
				leaf.LeafVersion)
		}

		spendPath := &arkscript.SpendPath{
			SpendInfo: &arkscript.SpendInfo{
				WitnessScript: leaf.Script,
				ControlBlock:  leaf.ControlBlock,
			},
		}
		err = spendPath.VerifyBindsToPkScript(
			p.ArkPSBT.Inputs[i].WitnessUtxo.PkScript,
		)
		if err != nil {
			return 0, fmt.Errorf("prepared ark input %d tapleaf "+
				"does not bind to witness utxo: %w", i, err)
		}

		val := btcutil.Amount(p.ArkPSBT.Inputs[i].WitnessUtxo.Value)
		if val <= 0 || val > btcutil.MaxSatoshi-arkInputTotal {
			return 0, fmt.Errorf("prepared ark input %d "+
				"value invalid", i)
		}
		arkInputTotal += val
	}

	return arkInputTotal, nil
}

// validatePreparedRecipients checks canonical recipient metadata, requires at
// least one asset-bearing output, and enforces fee-less satoshi conservation.
func (p *PreparedSubmitPackage) validatePreparedRecipients(
	recipients []oortx.RecipientOutput,
	inputTotal, arkInputTotal btcutil.Amount) error {

	canonicalRecipients := oortx.CanonicalRecipientOutputs(recipients)
	actualRecipients, err := ExtractArkRecipients(p.ArkPSBT)
	if err != nil {
		return err
	}
	if len(actualRecipients) != len(canonicalRecipients) {
		return fmt.Errorf("prepared recipient count mismatch")
	}

	var (
		assetRecipients int
		recipientTotal  btcutil.Amount
	)
	for i := range canonicalRecipients {
		recipient := canonicalRecipients[i]
		if recipient.Asset != nil {
			assetRecipients++
		}
		if err := recipient.ValidateAsset(); err != nil {
			return fmt.Errorf("prepared recipient %d: %w", i, err)
		}
		actual := actualRecipients[i]
		if actual.Value != recipient.Value ||
			!bytes.Equal(actual.PkScript, recipient.PkScript) {
			return fmt.Errorf("prepared recipient %d output "+
				"mismatch", i)
		}
		if recipient.Value <= 0 ||
			recipient.Value > btcutil.MaxSatoshi-recipientTotal {
			return fmt.Errorf("prepared recipient %d value invalid",
				i)
		}
		recipientTotal += recipient.Value
	}
	if assetRecipients == 0 {
		return fmt.Errorf("prepared recipients require at least one " +
			"asset-bearing output")
	}
	if arkInputTotal != inputTotal || recipientTotal != inputTotal {
		return fmt.Errorf("prepared package is not fee-less: inputs "+
			"%d sat, ark inputs %d sat, recipients %d sat",
			inputTotal, arkInputTotal, recipientTotal)
	}

	return nil
}

// expectedCheckpointPkScript derives the P2TR script committed to by the
// session checkpoint policy, owner leaf, and optional checkpoint Taproot Asset
// sibling root.
func expectedCheckpointPkScript(policy arkscript.CheckpointPolicy,
	ownerLeaf []byte, assetRoot *chainhash.Hash) ([]byte, error) {

	cpTapscript, err := arkscript.CheckpointTapScript(policy, ownerLeaf)
	if err != nil {
		return nil, err
	}
	leaves := make([]arkscript.PolicyLeaf, len(cpTapscript.Leaves))
	for i, l := range cpTapscript.Leaves {
		leaves[i] = arkscript.PolicyLeaf{Leaf: l}
	}
	compiled, err := arkscript.BuildTree(leaves, &arkscript.ARKNUMSKey)
	if err != nil {
		return nil, err
	}

	outputKey := compiled.OutputKey()
	if assetRoot != nil {
		composed, err := arkscript.ComposeWithSiblingRoot(
			compiled, *assetRoot,
		)
		if err != nil {
			return nil, err
		}
		outputKey = composed.OutputKey()
	}

	return txscript.PayToTaprootScript(outputKey)
}
