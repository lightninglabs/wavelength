package scripts

// VTXO Closure System:
//
// A VTXO (Virtual Transaction Output) is a taproot output built from a
// collection of closures. Each closure defines a spend condition as a
// tapscript leaf. The closure system is flexible - VTXOs can have any number
// of closures, enabling various spend paths and conditions.
//
// Closure Types:
//   - CSVSigClosure: Exit path with CSV timelock, single owner key
//   - CSVMultisigClosure: Exit path with CSV timelock, multiple keys
//   - MultisigClosure: Collaborative path requiring multiple signatures
//   - CLTVMultisigClosure: Collaborative path with absolute timelock
//   - ConditionMultisigClosure: Collaborative path with custom conditions
//
// Key Requirement:
//   Every VTXO MUST have at least one exit closure (CSVSigClosure or
//   CSVMultisigClosure) to ensure the owner can always unilaterally recover
//   funds after the timeout.
//
// NOTE: The exit vs collaborative distinction is semantic, not structural.
// Exit closures contain only owner key(s) - owner can spend unilaterally.
// Collaborative closures include signer key - require signer cooperation.
// The type-based categorization (ExitClosures/ForfeitClosures) is a
// simplification that works for the standard VTXO structure.
//
// Default VTXO Structure (created by NewDefaultVtxoScript):
//
//	                    Taproot Output
//	                   (NUMS Key Path)
//	                         |
//	            +------------+------------+
//	            |                         |
//	    [Exit Closure]           [Collab Closure]
//	    CSVSigClosure            MultisigClosure
//	            |                         |
//	  +-------------------+     +--------------------+
//	  | <exit_delay>       |     | Owner PK          |
//	  | OP_CSV             |     | OP_CHECKSIGVERIFY |
//	  | OP_DROP            |     | Cosigner PK       |
//	  | Owner PK           |     | OP_CHECKSIG       |
//	  | OP_CHECKSIG        |     +--------------------+
//	  +-------------------+
//
// Custom VTXOs can include additional closures with different conditions,
// multiple exit paths with varying delays, or conditional spend paths.

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/closure"
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
)

var (
	// ErrNoExitClosure is returned when a VTXO script has no exit closure.
	ErrNoExitClosure = fmt.Errorf("no exit closure found in vtxo script")

	// ErrNoCollabClosure is returned when a VTXO script has no
	// collaborative closure.
	ErrNoCollabClosure = fmt.Errorf("no collaborative closure found in " +
		"vtxo script")
)

// VTXOSpendData houses the necessary information needed to spend a VTXO
// output via either the collaborative multisig path or the timeout path.
type VTXOSpendData struct {
	// WitnessScript is the witness script for the leaf being spent.
	WitnessScript []byte

	// ControlBlock is the control block for the leaf being spent.
	ControlBlock []byte
}

// NewVtxoScript creates a VTXO script from the provided closures.
// Note: Callers should ensure at least one CSVMultisigClosure (exit closure)
// is included to allow unilateral recovery.
func NewVtxoScript(closures []closure.Closure) *closure.TapscriptsVtxoScript {
	return &closure.TapscriptsVtxoScript{Closures: closures}
}

// HasExitClosure returns true if the VTXO script contains at least one exit
// closure (CSVMultisigClosure). Every valid VTXO must have an exit closure.
func HasExitClosure(vtxoScript *closure.TapscriptsVtxoScript) bool {
	return len(vtxoScript.ExitClosures()) > 0
}

// NewDefaultVtxoScript creates a standard VTXO script with exit (CSV timeout)
// and collaborative (multisig) paths using the closure system.
func NewDefaultVtxoScript(owner, cosigner *btcec.PublicKey,
	exitDelay closure.RelativeLocktime) *closure.TapscriptsVtxoScript {

	return closure.NewDefaultVtxoScript(owner, cosigner, exitDelay)
}

// VtxoTapKey computes the taproot output key for a standard VTXO tapscript.
func VtxoTapKey(owner, cosigner *btcec.PublicKey,
	exitDelay closure.RelativeLocktime) (*btcec.PublicKey, error) {

	vtxoScript := closure.NewDefaultVtxoScript(owner, cosigner, exitDelay)
	key, _, err := vtxoScript.TapTree()
	return key, err
}

// VtxoPkScript returns the P2TR script for a VTXO.
func VtxoPkScript(owner, cosigner *btcec.PublicKey,
	exitDelay closure.RelativeLocktime) ([]byte, error) {

	key, err := VtxoTapKey(owner, cosigner, exitDelay)
	if err != nil {
		return nil, err
	}

	return txscript.PayToTaprootScript(key)
}

// NewVtxoSpendInfo derives the spend information for the specified closure
// index from the provided VTXO script.
func NewVtxoSpendInfo(vtxoScript *closure.TapscriptsVtxoScript,
	closureIndex int) (*VTXOSpendData, error) {

	proof, err := vtxoScript.GetSpendInfo(closureIndex)
	if err != nil {
		return nil, err
	}

	return &VTXOSpendData{
		WitnessScript: proof.Script,
		ControlBlock:  proof.ControlBlock,
	}, nil
}

// VtxoExitSpendInfo returns the spend info for the first exit (CSV timeout)
// closure found in the VTXO script. Returns ErrNoExitClosure if no exit
// closure exists. Exit closures include CSVSigClosure (single-sig) and
// CSVMultisigClosure (multi-sig).
func VtxoExitSpendInfo(vtxoScript *closure.TapscriptsVtxoScript) (*VTXOSpendData,
	error) {

	// Find the first exit closure by scanning all closures. Both
	// CSVSigClosure and CSVMultisigClosure are valid exit closures.
	for i, c := range vtxoScript.Closures {
		switch c.(type) {
		case *closure.CSVSigClosure, *closure.CSVMultisigClosure:
			return NewVtxoSpendInfo(vtxoScript, i)
		}
	}

	return nil, ErrNoExitClosure
}

// VtxoCollabSpendInfo returns the spend info for the first collaborative
// closure found in the VTXO script. Collaborative closures include
// MultisigClosure, CLTVMultisigClosure, and ConditionMultisigClosure.
// Returns ErrNoCollabClosure if no collaborative closure exists.
func VtxoCollabSpendInfo(vtxoScript *closure.TapscriptsVtxoScript) (
	*VTXOSpendData, error) {

	// Find the first collaborative closure by scanning all closures.
	for i, c := range vtxoScript.Closures {
		switch c.(type) {
		case *closure.MultisigClosure, *closure.CLTVMultisigClosure,
			*closure.ConditionMultisigClosure:

			return NewVtxoSpendInfo(vtxoScript, i)
		}
	}

	return nil, ErrNoCollabClosure
}

// CSVTimeoutTapLeaf constructs the tap leaf used as the timeout (exit) path
// for boarding or VTXO outputs using the closure system.
func CSVTimeoutTapLeaf(timeoutKey *btcec.PublicKey,
	csvDelay closure.RelativeLocktime) (txscript.TapLeaf, error) {

	csvClosure := &closure.CSVMultisigClosure{
		MultisigClosure: closure.MultisigClosure{
			PubKeys: []*btcec.PublicKey{timeoutKey},
			Type:    closure.MultisigTypeChecksig,
		},
		Locktime: csvDelay,
	}

	script, err := csvClosure.Script()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	return txscript.NewBaseTapLeaf(script), nil
}

// MultisigCollabTapLeaf returns the full tapscript leaf for the collaborative
// multisig script spend path between owner and cosigner using the closure
// system.
func MultisigCollabTapLeaf(ownerKey,
	cosignerKey *btcec.PublicKey) (txscript.TapLeaf, error) {

	multisigClosure := &closure.MultisigClosure{
		PubKeys: []*btcec.PublicKey{ownerKey, cosignerKey},
		Type:    closure.MultisigTypeChecksig,
	}

	script, err := multisigClosure.Script()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	return txscript.NewBaseTapLeaf(script), nil
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

// VtxoExitSpendWitness constructs the witness stack needed to spend the
// exit (timeout) path of a VTXO output.
func VtxoExitSpendWitness(signer input.Signer,
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

// SignVtxoCollabInput signs the collaborative multisig input of a VTXO
// output.
func SignVtxoCollabInput(signer input.Signer, tx *wire.MsgTx,
	inputIndex int, spendInfo *VTXOSpendData,
	keyDesc *keychain.KeyDescriptor, output *wire.TxOut,
	sigHashes *txscript.TxSigHashes,
	prevFetcher txscript.PrevOutputFetcher) (input.Signature, error) {

	signDesc := VTXOSignDesc(
		*keyDesc, output, sigHashes, prevFetcher, inputIndex, spendInfo,
	)

	return signer.SignOutputRaw(tx, signDesc)
}

// VtxoCollabSpendWitness constructs the witness stack needed to spend the
// collaborative (multisig) path of a VTXO output.
func VtxoCollabSpendWitness(ownerSig, cosignerSig input.Signature,
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
