package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
)

// validateSubmitOwnerProofs verifies that each checkpoint consumed by the Ark
// package uses the standard collaborative owner leaf and carries a valid owner
// signature for that leaf.
//
// Rebuild validation already proves the descriptors are consistent with the
// authoritative VTXO records. This function adds the missing possession proof:
// the submitter must show control of the claimed owner key before the server
// is allowed to take a shared lock on the corresponding VTXOs.
func validateSubmitOwnerProofs(ark *psbt.Packet,
	checkpoints []*psbt.Packet, descs []VTXOSigningDescriptor,
	checkpointPolicy arkscript.CheckpointPolicy) error {

	if ark == nil || ark.UnsignedTx == nil {
		return fmt.Errorf("ark psbt must be provided")
	}

	if checkpointPolicy.OperatorKey == nil {
		return fmt.Errorf("checkpoint operator key must be provided")
	}

	prevFetcher, err := arkPrevOutputFetcher(ark)
	if err != nil {
		return err
	}

	descByOutpoint := make(
		map[wire.OutPoint]VTXOSigningDescriptor, len(descs),
	)
	for _, desc := range descs {
		if _, exists := descByOutpoint[desc.Outpoint]; exists {
			return fmt.Errorf("duplicate signing descriptor for %s",
				desc.Outpoint)
		}

		descByOutpoint[desc.Outpoint] = desc
	}

	usedDescs := make(map[wire.OutPoint]struct{}, len(checkpoints))
	for _, checkpoint := range checkpoints {
		if checkpoint == nil || checkpoint.UnsignedTx == nil {
			return fmt.Errorf(
				"checkpoint psbt must include unsigned tx",
			)
		}

		if len(checkpoint.UnsignedTx.TxIn) != 1 ||
			len(checkpoint.Inputs) != 1 {

			return fmt.Errorf(
				"checkpoint must have exactly one input",
			)
		}

		outpoint := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
		desc, ok := descByOutpoint[outpoint]
		if !ok {
			return fmt.Errorf(
				"missing signing descriptor for %s",
				outpoint)
		}

		err := validateCheckpointOwnerProof(
			ark, checkpoint, checkpoint.UnsignedTx.TxHash(),
			desc,
			checkpointPolicy, prevFetcher,
		)
		if err != nil {
			return fmt.Errorf("checkpoint %s: %w",
				checkpoint.UnsignedTx.TxHash(), err)
		}

		usedDescs[outpoint] = struct{}{}
	}

	if len(usedDescs) != len(descByOutpoint) {
		return fmt.Errorf(
			"signing descriptors do not match checkpoint inputs",
		)
	}

	return nil
}

// validateCheckpointOwnerProof verifies that the Ark input spending a specific
// checkpoint output uses the expected collaborative leaf and carries a valid
// owner signature for it.
func validateCheckpointOwnerProof(ark *psbt.Packet,
	checkpoint *psbt.Packet, checkpointTxid chainhash.Hash,
	desc VTXOSigningDescriptor,
	checkpointPolicy arkscript.CheckpointPolicy,
	prevFetcher txscript.PrevOutputFetcher) error {

	arkInputIndex, arkInput, err := findArkInputByCheckpointTxid(
		ark, checkpointTxid,
	)
	if err != nil {
		return err
	}

	if arkInput.WitnessUtxo == nil {
		return fmt.Errorf("ark input missing witness utxo")
	}

	ownerLeafScript, err := findOwnerLeafScript(
		ark, arkInputIndex, checkpoint, desc, checkpointPolicy,
	)
	if err != nil {
		return fmt.Errorf("find owner leaf: %w", err)
	}

	leaf, err := findTapLeafByScript(arkInput, ownerLeafScript)
	if err != nil {
		return fmt.Errorf("missing owner leaf: %w", err)
	}

	leafHash := txscript.NewTapLeaf(
		leaf.LeafVersion, leaf.Script,
	).TapHash()

	err = validateTapLeafControlBlockBinding(leaf, arkInput.WitnessUtxo)
	if err != nil {
		return fmt.Errorf("invalid owner leaf binding: %w", err)
	}

	ownerKey, err := findCollaborativeOwnerKey(
		desc.OwnerLeafPolicy, checkpointPolicy.OperatorKey,
	)
	if err != nil {
		return err
	}

	ownerSig, err := findSubmitOwnerSignature(
		arkInput, ownerKey, leafHash[:],
	)
	if err != nil {
		return err
	}

	err = verifyTaprootScriptSpendSig(
		ark.UnsignedTx, arkInputIndex, prevFetcher, leaf, ownerSig,
	)
	if err != nil {
		return fmt.Errorf("owner signature invalid: %w", err)
	}

	return nil
}

// arkPrevOutputFetcher builds the full prevout context for Ark input
// signature verification. BIP-341 sighashes commit to every input's prevout,
// so multi-input Ark transactions must not be verified with a canned fetcher.
func arkPrevOutputFetcher(ark *psbt.Packet) (txscript.PrevOutputFetcher,
	error) {

	if ark == nil || ark.UnsignedTx == nil {
		return nil, fmt.Errorf("ark psbt must be provided")
	}

	if len(ark.Inputs) != len(ark.UnsignedTx.TxIn) {
		return nil, fmt.Errorf("ark psbt input count mismatch: "+
			"tx=%d psbt=%d", len(ark.UnsignedTx.TxIn),
			len(ark.Inputs))
	}

	prevOuts := make(
		map[wire.OutPoint]*wire.TxOut, len(ark.UnsignedTx.TxIn),
	)
	for i, txIn := range ark.UnsignedTx.TxIn {
		witnessUtxo := ark.Inputs[i].WitnessUtxo
		if witnessUtxo == nil {
			return nil, fmt.Errorf(
				"ark input %d missing witness utxo", i,
			)
		}

		prevOuts[txIn.PreviousOutPoint] = witnessUtxo
	}

	return txscript.NewMultiPrevOutFetcher(prevOuts), nil
}

// findArkInputByCheckpointTxid locates the Ark input that spends checkpoint
// txid, rejecting missing or duplicate references.
func findArkInputByCheckpointTxid(ark *psbt.Packet,
	checkpointTxid chainhash.Hash) (int, *psbt.PInput, error) {

	if ark == nil || ark.UnsignedTx == nil {
		return 0, nil, fmt.Errorf("ark psbt must be provided")
	}

	var matchIndex = -1
	for i, txIn := range ark.UnsignedTx.TxIn {
		if txIn.PreviousOutPoint.Hash != checkpointTxid {
			continue
		}

		if matchIndex >= 0 {
			return 0, nil, fmt.Errorf("multiple ark inputs spend "+
				"checkpoint %s", checkpointTxid)
		}

		matchIndex = i
	}

	if matchIndex < 0 {
		return 0, nil, fmt.Errorf(
			"ark input for checkpoint %s not found",
			checkpointTxid)
	}

	if matchIndex >= len(ark.Inputs) {
		return 0, nil, fmt.Errorf("ark input metadata missing for %s",
			checkpointTxid)
	}

	return matchIndex, &ark.Inputs[matchIndex], nil
}

// findCollaborativeOwnerKey extracts the single non-operator participant from
// the collaborative owner-leaf policy.
func findCollaborativeOwnerKey(ownerLeafPolicy []byte,
	operatorKey *btcec.PublicKey) (*btcec.PublicKey, error) {

	if operatorKey == nil {
		return nil, fmt.Errorf(
			"checkpoint operator key must be provided",
		)
	}
	if len(ownerLeafPolicy) == 0 {
		return nil, fmt.Errorf("owner leaf policy not found")
	}

	leaf, err := arkscript.DecodeLeafTemplate(ownerLeafPolicy)
	if err != nil {
		return nil, fmt.Errorf("decode owner leaf policy: %w", err)
	}

	wantOperator := schnorr.SerializePubKey(operatorKey)
	var ownerKey *btcec.PublicKey

	for _, key := range leaf.ParticipantKeys() {
		if key == nil {
			continue
		}

		if bytes.Equal(schnorr.SerializePubKey(key), wantOperator) {
			continue
		}

		if ownerKey != nil {
			return nil, fmt.Errorf(
				"owner leaf policy must contain " +
					"exactly one non-operator key",
			)
		}

		ownerKey = key
	}

	if ownerKey == nil {
		return nil, fmt.Errorf("owner leaf policy must contain " +
			"exactly one non-operator key")
	}

	return ownerKey, nil
}

// findSubmitOwnerSignature locates the collaborative-leaf signature for the
// claimed owner key on an Ark input.
func findSubmitOwnerSignature(in *psbt.PInput, ownerKey *btcec.PublicKey,
	leafHash []byte) (*psbt.TaprootScriptSpendSig, error) {

	if in == nil {
		return nil, fmt.Errorf("psbt input must be provided")
	}
	if ownerKey == nil {
		return nil, fmt.Errorf("owner key must be provided")
	}

	wantPub := schnorr.SerializePubKey(ownerKey)

	for i := range in.TaprootScriptSpendSig {
		sigRec := in.TaprootScriptSpendSig[i]
		if sigRec == nil {
			continue
		}

		if !bytes.Equal(sigRec.XOnlyPubKey, wantPub) {
			continue
		}

		if !bytes.Equal(sigRec.LeafHash, leafHash) {
			continue
		}

		return sigRec, nil
	}

	return nil, fmt.Errorf("missing owner signature for collaborative leaf")
}

// findTapLeafByScript locates the tapleaf script/control block for the
// specified raw script bytes within a PSBT input.
func findTapLeafByScript(in *psbt.PInput,
	script []byte) (*psbt.TaprootTapLeafScript, error) {

	if in == nil {
		return nil, fmt.Errorf("psbt input must be provided")
	}

	for i := range in.TaprootLeafScript {
		leaf := in.TaprootLeafScript[i]
		if leaf == nil {
			continue
		}

		if bytes.Equal(leaf.Script, script) {
			return leaf, nil
		}
	}

	return nil, fmt.Errorf("tap leaf script not found")
}
