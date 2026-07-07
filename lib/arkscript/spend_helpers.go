package arkscript

import (
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	// CollabMultisigLeafWitnessSize is the estimated witness size for
	// the collaborative spend path of a boarding or vtxo output.
	CollabMultisigLeafWitnessSize = lntypes.WeightUnit(1 + 64 + 1 + 64)

	// UnilateralTimeoutLeafWitnessSize is the estimated witness size
	// for the unilateral timeout path of a boarding or vtxo output.
	UnilateralTimeoutLeafWitnessSize = lntypes.WeightUnit(1 + 64)
)

// BuildSignDescriptor returns the sign descriptor needed to sign for a
// leaf path using the provided spend info. The key descriptor should
// correspond to the key needed for the specific leaf being signed.
func (s *SpendInfo) BuildSignDescriptor(keyDesc keychain.KeyDescriptor,
	output *wire.TxOut, sigHashes *txscript.TxSigHashes,
	prevFetcher txscript.PrevOutputFetcher,
	inputIndex int) *input.SignDescriptor {

	return &input.SignDescriptor{
		KeyDesc:           keyDesc,
		SignMethod:        input.TaprootScriptSpendSignMethod,
		Output:            output,
		HashType:          txscript.SigHashDefault,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevFetcher,
		InputIndex:        inputIndex,
		WitnessScript:     s.WitnessScript,
		ControlBlock:      s.ControlBlock,
	}
}

// CollabWitness constructs the witness stack needed to spend the
// collaborative path of a VTXO output using the two signatures.
//
// The witness stack ordering is:
//
//	<cosigner_sig> <owner_sig> <script> <control_block>
func (s *SpendInfo) CollabWitness(ownerSig, cosignerSig input.Signature) (
	wire.TxWitness, error) {

	if ownerSig == nil || cosignerSig == nil {
		return nil, fmt.Errorf("owner and cosigner signatures must " +
			"be populated for a collaborative spend")
	}

	if len(s.WitnessScript) == 0 || len(s.ControlBlock) == 0 {
		return nil, fmt.Errorf("collaborative spend info must " +
			"contain witness script and control block")
	}

	witnessStack := make(wire.TxWitness, 4)
	witnessStack[0] = cosignerSig.Serialize()
	witnessStack[1] = ownerSig.Serialize()
	witnessStack[2] = s.WitnessScript
	witnessStack[3] = s.ControlBlock

	return witnessStack, nil
}

// TimeoutWitness constructs the witness stack needed to spend the timeout
// path of a VTXO output.
//
// The witness stack ordering is:
//
//	<owner_sig> <script> <control_block>
func (s *SpendInfo) TimeoutWitness(sig input.Signature) (wire.TxWitness,
	error) {

	if sig == nil {
		return nil, fmt.Errorf("signature must be provided")
	}

	if len(s.WitnessScript) == 0 || len(s.ControlBlock) == 0 {
		return nil, fmt.Errorf("witness script and control block " +
			"must be populated to construct timeout witness")
	}

	witnessStack := make(wire.TxWitness, 3)
	witnessStack[0] = sig.Serialize()
	witnessStack[1] = s.WitnessScript
	witnessStack[2] = s.ControlBlock

	return witnessStack, nil
}

// UnilateralCSVTimeoutTapLeaf constructs the tap leaf used as the timeout
// path for boarding or VTXO outputs.
//
// The canonical script encoding matches the arkscript CSV node:
//
//	<timeout_key_xonly> OP_CHECKSIG <exit_delay> OP_CSV OP_DROP
//
// The csvDelay parameter is a raw block count; it is converted to the
// BIP-68 block-mode sequence encoding before being stored on the leaf.
func UnilateralCSVTimeoutTapLeaf(timeoutKey *btcec.PublicKey,
	csvDelay uint32) (txscript.TapLeaf, error) {

	csvNode := &CSV{
		Lock: blockchain.LockTimeToSequence(false, csvDelay),
		Inner: &Multisig{
			Keys: []*btcec.PublicKey{
				timeoutKey,
			},
		},
	}

	script, err := csvNode.Script()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	return txscript.NewBaseTapLeaf(script), nil
}

// MultiSigCollabTapLeaf returns the full tapscript leaf for the
// collaborative multisig script spend path between owner and cosigner.
//
// The canonical script encoding matches the arkscript Multisig node:
//
//	<owner_key_xonly> OP_CHECKSIGVERIFY <cosigner_key_xonly> OP_CHECKSIG
func MultiSigCollabTapLeaf(ownerKey, cosignerKey *btcec.PublicKey) (
	txscript.TapLeaf, error) {

	multisigNode := &Multisig{
		Keys: []*btcec.PublicKey{
			ownerKey,
			cosignerKey,
		},
	}

	script, err := multisigNode.Script()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	return txscript.NewBaseTapLeaf(script), nil
}

// VTXOTapScript constructs the full tapscript for a VTXO type output. This
// output structure is used for both boarding UTXOs as well as VTXOs. The
// tree consists of:
//   - an unspendable NUMS keypath.
//   - Collaborative spend path between owner and cosigner.
//   - Timeout path allowing the owner to recover funds after a CSV exit
//     delay.
func VTXOTapScript(ownerKey, cosignerKey *btcec.PublicKey, exitDelay uint32) (
	*waddrmgr.Tapscript, error) {

	collabLeaf, err := MultiSigCollabTapLeaf(ownerKey, cosignerKey)
	if err != nil {
		return nil, err
	}

	timeoutLeaf, err := UnilateralCSVTimeoutTapLeaf(ownerKey, exitDelay)
	if err != nil {
		return nil, err
	}

	tapscript := input.TapscriptFullTree(
		&ARKNUMSKey, collabLeaf, timeoutLeaf,
	)

	// Compute and set the root hash since TapscriptFullTree doesn't
	// populate it. Callers need this to construct taproot addresses.
	tree := txscript.AssembleTaprootScriptTree(tapscript.Leaves...)
	rootHash := tree.RootNode.TapHash()
	tapscript.RootHash = rootHash[:]

	return tapscript, nil
}

// VTXOTapKey computes the taproot output key for a standard VTXO tapscript.
func VTXOTapKey(ownerKey, cosignerKey *btcec.PublicKey, exitDelay uint32) (
	*btcec.PublicKey, error) {

	vp, err := NewVTXOPolicy(ownerKey, cosignerKey, exitDelay)
	if err != nil {
		return nil, err
	}

	return vp.OutputKey(), nil
}

// MaybeAppendSighash appends a sighash type to the end of a signature if
// the sighash type isn't sighash default.
func MaybeAppendSighash(sig input.Signature,
	sigHash txscript.SigHashType) []byte {

	sigBytes := sig.Serialize()
	if sigHash == txscript.SigHashDefault {
		return sigBytes
	}

	return append(sigBytes, byte(sigHash))
}

// SignVTXOCollabInput signs the collaborative multisig input of a VTXO
// output.
func SignVTXOCollabInput(signer input.Signer, tx *wire.MsgTx, inputIndex int,
	spendInfo *SpendInfo, keyDesc *keychain.KeyDescriptor,
	output *wire.TxOut, sigHashes *txscript.TxSigHashes,
	prevFetcher txscript.PrevOutputFetcher) (input.Signature, error) {

	signDesc := spendInfo.BuildSignDescriptor(
		*keyDesc, output, sigHashes, prevFetcher, inputIndex,
	)

	return signer.SignOutputRaw(tx, signDesc)
}

// VTXOTimeoutSpendWitness constructs the witness stack needed to spend
// the timeout path of a VTXO output.
func VTXOTimeoutSpendWitness(signer input.Signer,
	signDesc *input.SignDescriptor,
	sweepTx *wire.MsgTx) (wire.TxWitness, error) {

	if len(signDesc.WitnessScript) == 0 ||
		len(signDesc.ControlBlock) == 0 {
		return nil, fmt.Errorf("witness script and control block " +
			"must be populated in sign desc to sign a timeout path")
	}

	timeoutSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	witnessStack := make(wire.TxWitness, 3)
	witnessStack[0] = MaybeAppendSighash(
		timeoutSig, signDesc.HashType,
	)
	witnessStack[1] = signDesc.WitnessScript
	witnessStack[2] = signDesc.ControlBlock

	return witnessStack, nil
}

// NewVTXOSpendInfoFromPolicy derives the spend information for the
// specified leaf index from a VTXOPolicy. This is a convenience wrapper
// for callers migrating from the scripts.NewVTXOSpendInfo API.
//
// IMPORTANT: The leafIndex here uses the legacy semantic ordering
// (0=collab, 1=exit), NOT the canonical VTXOPolicy.Leaves index.
// New callers should use VTXOPolicy.CollabSpendInfo/ExitSpendInfo
// directly instead.
func NewVTXOSpendInfoFromPolicy(ownerKey, cosignerKey *btcec.PublicKey,
	exitDelay uint32, leafIndex int) (*SpendInfo, error) {

	vp, err := NewVTXOPolicy(ownerKey, cosignerKey, exitDelay)
	if err != nil {
		return nil, err
	}

	switch leafIndex {
	case 0:
		return vp.CollabSpendInfo()

	case 1:
		return vp.ExitSpendInfo()

	default:
		return nil, fmt.Errorf("leaf index %d out of range for "+
			"standard VTXO", leafIndex)
	}
}
