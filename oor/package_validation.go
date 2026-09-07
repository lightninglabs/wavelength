package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
)

// validateIncomingPackage executes every finalized checkpoint spend before any
// untrusted incoming artifact enters the receive state machine or recovery
// store. Structural validation alone accepts correctly sized garbage
// signatures, which would defer failure until unilateral exit.
func validateIncomingPackage(label string, sessionID SessionID,
	ark *psbt.Packet, checkpoints []*psbt.Packet) error {

	if sessionID == (SessionID{}) {
		return fmt.Errorf("%s session id must be provided", label)
	}

	if ark == nil || ark.UnsignedTx == nil {
		return fmt.Errorf("%s ark psbt must be provided", label)
	}

	txid := ark.UnsignedTx.TxHash()
	if SessionID(txid) != sessionID {
		return fmt.Errorf("%s session id does not match ark txid",
			label)
	}

	err := oortx.ValidateFinalizePackageSigned(ark, checkpoints)
	if err != nil {
		return fmt.Errorf("%s finalize package invalid: %w", label, err)
	}

	return nil
}

// validateIncomingPackageGraph validates that incoming ancestor artifacts are
// finalized packages and that each supplied ancestor is reachable from the
// received package's checkpoint-input chain. This avoids persisting unrelated
// operator/indexer-supplied packages as recovery material.
func validateIncomingPackageGraph(root PackageArtifact,
	ancestors []PackageArtifact) error {

	if err := validateIncomingPackage(
		"incoming package", root.SessionID, root.ArkPSBT,
		root.FinalCheckpointPSBTs,
	); err != nil {
		return err
	}

	ancestorBySession := make(map[SessionID]PackageArtifact, len(ancestors))
	for i := range ancestors {
		ancestor := ancestors[i]
		label := fmt.Sprintf("ancestor package %d", i)

		if err := validateIncomingPackage(
			label, ancestor.SessionID, ancestor.ArkPSBT,
			ancestor.FinalCheckpointPSBTs,
		); err != nil {
			return err
		}

		if ancestor.SessionID == root.SessionID {
			return fmt.Errorf("%s duplicates incoming package",
				label)
		}

		if _, ok := ancestorBySession[ancestor.SessionID]; ok {
			return fmt.Errorf("duplicate ancestor package %s",
				ancestor.SessionID.String())
		}

		ancestorBySession[ancestor.SessionID] = ancestor
	}

	reachable := make(map[SessionID]struct{}, len(ancestors))
	walkPackageAncestors(root, ancestorBySession, reachable)

	for i := range ancestors {
		ancestor := ancestors[i]
		if _, ok := reachable[ancestor.SessionID]; !ok {
			return fmt.Errorf("ancestor package %s is not "+
				"consumed by incoming package chain",
				ancestor.SessionID.String())
		}
	}

	return nil
}

// walkPackageAncestors marks ancestor packages reachable from pkg's finalized
// checkpoints by following checkpoint input prevouts that spend ancestor Ark
// outputs.
func walkPackageAncestors(pkg PackageArtifact,
	ancestorBySession map[SessionID]PackageArtifact,
	reachable map[SessionID]struct{}) {

	for i := range pkg.FinalCheckpointPSBTs {
		checkpoint := pkg.FinalCheckpointPSBTs[i]
		if checkpoint == nil || checkpoint.UnsignedTx == nil ||
			len(checkpoint.UnsignedTx.TxIn) == 0 {

			continue
		}

		// Finalized OOR checkpoints are single-input collab spends.
		// oortx.ValidateFinalizePackageSigned enforces that invariant
		// before this graph walk, so TxIn[0] is the only ancestry
		// edge to follow.
		prevOut := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
		parentID := SessionID(prevOut.Hash)
		ancestor, ok := ancestorBySession[parentID]
		if !ok {
			continue
		}

		if !validAncestorOutput(
			ancestor.ArkPSBT, checkpoint, prevOut.Index,
		) {

			continue
		}

		if _, ok := reachable[parentID]; ok {
			continue
		}

		reachable[parentID] = struct{}{}
		walkPackageAncestors(ancestor, ancestorBySession, reachable)
	}
}

// validAncestorOutput reports whether a checkpoint input's declared witness
// UTXO equals the real non-anchor Ark output in the referenced ancestor.
func validAncestorOutput(ark, checkpoint *psbt.Packet,
	outputIndex uint32) bool {

	if ark == nil || ark.UnsignedTx == nil || checkpoint == nil ||
		len(checkpoint.Inputs) != 1 ||
		checkpoint.Inputs[0].WitnessUtxo == nil {
		return false
	}

	tx := ark.UnsignedTx
	if outputIndex >= uint32(len(tx.TxOut)) {
		return false
	}

	ancestorOutput := tx.TxOut[outputIndex]
	if ancestorOutput == nil || arktx.IsAnchorOutput(ancestorOutput) {
		return false
	}

	witnessUtxo := checkpoint.Inputs[0].WitnessUtxo

	return ancestorOutput.Value == witnessUtxo.Value &&
		bytes.Equal(ancestorOutput.PkScript, witnessUtxo.PkScript)
}

// packageArtifactForValidation projects package fields into the common
// validation shape.
func packageArtifactForValidation(sessionID SessionID, ark *psbt.Packet,
	checkpoints []*psbt.Packet) PackageArtifact {

	return PackageArtifact{
		SessionID:            sessionID,
		ArkPSBT:              ark,
		FinalCheckpointPSBTs: checkpoints,
	}
}
