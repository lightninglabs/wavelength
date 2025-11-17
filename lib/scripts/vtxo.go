package scripts

// VTXO Taproot Tree Structure:
//
// - The "Owner" is the participant who is able to unilaterally recover funds
//   after the CSV delay expires.
// - The "Cosigner" is the participant who collaborates with the owner to spend
//   the output at any time.
//
//	                    Taproot Output
//	                   (NUMS Key Path)
//	                         |
//	            +------------+------------+
//	            |                         |
//	    [Collaborative Path]      [Timeout Path]
//	    (Leaf Index: 0)           (Leaf Index: 1)
//	            |                         |
//	  +-------------------+     +--------------------+
//	  | Owner PK          |     | Owner PK          |
//	  | OP_CHECKSIGVERIFY |     | OP_CHECKSIG        |
//	  | Cosigner PK       |     | <exit_delay>       |
//	  | OP_CHECKSIG       |     | OP_CSV             |
//	  +-------------------+     | OP_DROP            |
//	                            +--------------------+
//
// Spending Paths:
//   - Collaborative: Both owner and cosigner signatures required (anytime)
//   - Timeout: Owner signature only (after CSV delay expires)
//   - Key Path: Unspendable (NUMS point with no known discrete log)

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	// CollabMultisigLeafWitnessSize is the estimated witness size for the
	// collaborative spend path of a boarding or vtxo output.
	// The size is calculated as:
	//
	// 1 + 64 bytes for owner signature + length byte
	// 1 + 64 bytes for cosigner signature + length byte.
	CollabMultisigLeafWitnessSize = lntypes.WeightUnit(1 + 64 + 1 + 64)

	// UnilateralTimeoutLeafWitnessSize is the estimated witness size for
	// the unilateral timeout path of a boarding or vtxo output.
	// The size is calculated as:
	//
	// 1 + 64 bytes for owner signature + length byte.
	UnilateralTimeoutLeafWitnessSize = lntypes.WeightUnit(1 + 64)

	// vtxoCollabPathLeafIndex is the index of the collaborative multisig
	// path leaf in the VTXO tapscript tree.
	vtxoCollabPathLeafIndex = 0

	// vtxoTimeoutPathLeafIndex is the index of the timeout path leaf in the
	// VTXO tapscript tree.
	vtxoTimeoutPathLeafIndex = 1
)

// VTXOLeafType is an enum-like type to identify the different leaves in a VTXO
// tapscript tree.
type VTXOLeafType int

const (
	// VTXOCollabPathLeaf is the leaf type for the collaborative multisig
	// path in a VTXO tapscript tree.
	VTXOCollabPathLeaf VTXOLeafType = iota

	// VTXOTimeoutPathLeaf is the leaf type for the timeout path in a VTXO
	// tapscript tree.
	VTXOTimeoutPathLeaf
)

// LeafIndex returns the index of the VTXO leaf type in the tapscript tree.
func (l VTXOLeafType) LeafIndex() (int, error) {
	switch l {
	case VTXOCollabPathLeaf:
		return vtxoCollabPathLeafIndex, nil

	case VTXOTimeoutPathLeaf:
		return vtxoTimeoutPathLeafIndex, nil

	default:
		return 0, fmt.Errorf("unknown VTXO leaf type: %d", l)
	}
}

// VTXOTapScript constructs the full tapscript for a VTXO type output. This
// output structure is used for both boarding UTXOs as well as VTXOs. The tree
// consists of:
//   - an unspendable NUMS keypath.
//   - Collaborative spend path between owner and cosigner.
//   - Timeout path allowing the owner to recover funds after a CSV exit delay.
func VTXOTapScript(ownerKey, cosignerKey *btcec.PublicKey,
	exitDelay uint32) (*waddrmgr.Tapscript, error) {

	collabLeaf, err := MultiSigCollabTapLeaf(ownerKey, cosignerKey)
	if err != nil {
		return nil, err
	}

	// Timeout path is always controlled by the owner.
	timeoutLeaf, err := UnilateralCSVTimeoutTapLeaf(ownerKey, exitDelay)
	if err != nil {
		return nil, err
	}

	return input.TapscriptFullTree(&ARKNUMSKey, collabLeaf, timeoutLeaf),
		nil
}

// VTXOTapKey computes the taproot output key for a standard VTXO tapscript.
// The timeout path is controlled by the owner key.
func VTXOTapKey(ownerKey, cosignerKey *btcec.PublicKey,
	exitDelay uint32) (*btcec.PublicKey, error) {

	vtxoTapscript, err := VTXOTapScript(
		ownerKey, cosignerKey, exitDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create VTXO tapscript: %w",
			err)
	}

	// Compute the taproot output key from the internal key and tree root.
	tree := txscript.AssembleTaprootScriptTree(vtxoTapscript.Leaves...)
	rootHash := tree.RootNode.TapHash()
	outputKey := txscript.ComputeTaprootOutputKey(
		vtxoTapscript.ControlBlock.InternalKey, rootHash[:],
	)

	return outputKey, nil
}

// VTXOSpendData houses the necessary information needed to spend a VTXO
// output via either the collaborative multisig path or the timeout path.
type VTXOSpendData struct {
	// WitnessScript is the witness script for the leaf being spent.
	WitnessScript []byte

	// ControlBlock is the control block for the leaf being spent.
	ControlBlock []byte
}

// NewVTXOSpendInfo derives the spend information for the specified leaf
// type from the provided tapscript.
func NewVTXOSpendInfo(tapscript *waddrmgr.Tapscript,
	leaf VTXOLeafType) (*VTXOSpendData, error) {

	leafIndex, err := leaf.LeafIndex()
	if err != nil {
		return nil, err
	}

	// Ensure the leaf index is within bounds before accessing.
	if len(tapscript.Leaves) <= leafIndex {
		return nil, fmt.Errorf("leaf index %d out of bounds, "+
			"tapscript has %d leaves", leafIndex,
			len(tapscript.Leaves))
	}

	// Get the leaf.
	targetLeaf := tapscript.Leaves[leafIndex]

	// Derive the full tap tree to extract the control block for the
	// target leaf.
	tapTree := txscript.AssembleTaprootScriptTree(tapscript.Leaves...)
	if len(tapTree.LeafMerkleProofs) <= leafIndex {
		return nil, fmt.Errorf("missing taproot proof for vtxo leaf")
	}

	leafProof := tapTree.LeafMerkleProofs[leafIndex]

	controlBlock := leafProof.ToControlBlock(&ARKNUMSKey)
	ctrlBytes, err := controlBlock.ToBytes()
	if err != nil {
		return nil, err
	}

	return &VTXOSpendData{
		WitnessScript: targetLeaf.Script,
		ControlBlock:  ctrlBytes,
	}, nil
}

// VTXOSignDesc returns the sign descriptor needed to sign for any leaf path
// in a VTXO output. The key descriptor provided should correspond to the key
// needed for the specific leaf & pub key being signed for.
func VTXOSignDesc(keyDesc keychain.KeyDescriptor, output *wire.TxOut,
	sigHashes *txscript.TxSigHashes, prevFetcher txscript.PrevOutputFetcher,
	inputIndex int, spendInfo *VTXOSpendData) *input.SignDescriptor {

	return &input.SignDescriptor{
		KeyDesc:           keyDesc,
		SignMethod:        input.TaprootScriptSpendSignMethod,
		Output:            output,
		HashType:          txscript.SigHashDefault,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevFetcher,
		InputIndex:        inputIndex,
		WitnessScript:     spendInfo.WitnessScript,
		ControlBlock:      spendInfo.ControlBlock,
	}
}

// VTXOTimeoutSpendWitness constructs the witness stack needed to spend the
// timeout path of a VTXO output.
func VTXOTimeoutSpendWitness(signer input.Signer,
	signDesc *input.SignDescriptor, sweepTx *wire.MsgTx) (wire.TxWitness,
	error) {

	// First, we'll ensure that the sign descriptor has all the necessary
	// information populated to sign for the timeout path. The timeout
	// path would always be spent by the creator of the VTXO and so they
	// should have all the necessary information.
	if len(signDesc.WitnessScript) == 0 || len(signDesc.ControlBlock) == 0 {
		return nil, fmt.Errorf("witness script and control block " +
			"must be populated in sign desc to sign a timeout path")
	}

	// First, we'll generate the sweep signature based on the populated
	// sign desc. This should give us a valid schnorr signature for the
	// timeout path leaf.
	timeoutSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// Construct the final witness needed to spend the script:
	//
	//  <owner timeout sig> <timeout script> <control_block>
	witnessStack := make(wire.TxWitness, 3)
	witnessStack[0] = maybeAppendSighash(timeoutSig, signDesc.HashType)
	witnessStack[1] = signDesc.WitnessScript
	witnessStack[2] = signDesc.ControlBlock

	return witnessStack, nil
}

// SignVTXOCollabInput signs the collaborative multisig input of a VTXO output.
func SignVTXOCollabInput(signer input.Signer, tx *wire.MsgTx,
	inputIndex int, spendInfo *VTXOSpendData,
	keyDesc *keychain.KeyDescriptor, output *wire.TxOut,
	sigHashes *txscript.TxSigHashes,
	prevFetcher txscript.PrevOutputFetcher) (input.Signature, error) {

	signDesc := VTXOSignDesc(
		*keyDesc, output, sigHashes, prevFetcher, inputIndex, spendInfo,
	)

	return signer.SignOutputRaw(tx, signDesc)
}

// VTXOCollabSpendWitness constructs the witness stack needed to spend the
// collaborative path of a VTXO output.
func VTXOCollabSpendWitness(ownerSig, cosignerSig input.Signature,
	spendInfo *VTXOSpendData) (wire.TxWitness, error) {

	if ownerSig == nil || cosignerSig == nil {
		return nil, fmt.Errorf("owner and cosigner signatures must " +
			"be populated for a collaborative spend")
	}

	if spendInfo == nil || len(spendInfo.WitnessScript) == 0 ||
		len(spendInfo.ControlBlock) == 0 {

		return nil, fmt.Errorf("collaborative spend info must " +
			"contain witness script and control block")
	}

	witnessStack := make(wire.TxWitness, 4)
	witnessStack[0] = cosignerSig.Serialize()
	witnessStack[1] = ownerSig.Serialize()
	witnessStack[2] = spendInfo.WitnessScript
	witnessStack[3] = spendInfo.ControlBlock

	return witnessStack, nil
}

// UnilateralCSVTimeoutTapLeaf constructs the tap leaf used as the timeout path
// for boarding or VTXO outputs.
//
// The final script used is:
//
//	<timeout_key> OP_CHECKSIG
//	<exit_delay>  OP_CHECKSEQUENCEVERIFY OP_DROP
func UnilateralCSVTimeoutTapLeaf(timeoutKey *btcec.PublicKey,
	csvDelay uint32) (txscript.TapLeaf, error) {

	// Use ScriptTemplate to construct the timeout script. This script
	// ensures the proper party can sign for this output, and that the CSV
	// delay has been upheld.
	secondLevelLeafScript, err := txscript.ScriptTemplate(`
		{{ hex .ExitKey }} OP_CHECKSIG
		{{.CSVDelay}} OP_CHECKSEQUENCEVERIFY OP_DROP`,
		txscript.WithScriptTemplateParams(map[string]interface{}{
			"ExitKey":  schnorr.SerializePubKey(timeoutKey),
			"CSVDelay": int64(csvDelay),
		}),
	)
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	return txscript.NewBaseTapLeaf(secondLevelLeafScript), nil
}

// MultiSigCollabTapLeaf returns the full tapscript leaf for the collaborative
// multisig script spend path between owner and cosigner. This is used for both
// boarding and VTXO outputs.
//
// The final script used is:
//
//	<owner_key>    OP_CHECKSIGVERIFY
//	<cosigner_key> OP_CHECKSIG
func MultiSigCollabTapLeaf(ownerKey,
	cosignerKey *btcec.PublicKey) (txscript.TapLeaf, error) {

	// Use ScriptTemplate to construct the collaborative multisig script.
	// This script requires both owner and cosigner signatures to spend.
	timeoutLeafScript, err := txscript.ScriptTemplate(`
		{{ hex .OwnerKey }} OP_CHECKSIGVERIFY
		{{ hex .CosignerKey }} OP_CHECKSIG`,
		txscript.WithScriptTemplateParams(map[string]interface{}{
			"OwnerKey":    schnorr.SerializePubKey(ownerKey),
			"CosignerKey": schnorr.SerializePubKey(cosignerKey),
		}),
	)
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	return txscript.NewBaseTapLeaf(timeoutLeafScript), nil
}

// maybeAppendSighashType appends a sighash type to the end of a signature if
// the sighash type isn't sighash default.
func maybeAppendSighash(sig input.Signature,
	sigHash txscript.SigHashType) []byte {

	sigBytes := sig.Serialize()
	if sigHash == txscript.SigHashDefault {
		return sigBytes
	}

	return append(sigBytes, byte(sigHash))
}
