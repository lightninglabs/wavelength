package lib

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	// BoardingCollabPathLeafIndex is the index of the collaborative multisig
	// path.
	BoardingCollabPathLeafIndex = 0

	// BoardingTimeoutPathLeafIndex is the index of the CSV timeout path.
	BoardingTimeoutPathLeafIndex = 1
)

// BoardingTapScript constructs the full tapscript for a boarding output. The
// leaves of the tapscript are:
// - Collaborative spend path between client and server.
// - Timeout path allowing client to recover funds after exit delay.
func BoardingTapScript(clientKey, serverKey *btcec.PublicKey,
	exitDelay uint32) (*waddrmgr.Tapscript, error) {

	collabLeaf, err := MultiSigCollabTapLeaf(clientKey, serverKey)
	if err != nil {
		return nil, err
	}

	timeoutLeaf, err := UnilateralCSVTimeoutTapLeaf(clientKey, exitDelay)
	if err != nil {
		return nil, err
	}

	return input.TapscriptFullTree(&ARKNUMSKey, collabLeaf, timeoutLeaf),
		nil
}

// BoardingCollabSpendInfo bundles the witness script and control block that are
// required to spend the collaborative tapscript leaf of a boarding output.
type BoardingCollabSpendInfo struct {
	WitnessScript []byte
	ControlBlock  []byte
}

// BoardingTimeoutSpendInfo bundles the timeout path script, tap tree and
// control block information.
type BoardingTimeoutSpendInfo struct {
	WitnessScript []byte
	ControlBlock  []byte
}

// NewBoardingCollabSpendInfo derives the collaborative spend information from
// the provided tapscript. This avoids duplicating the taproot tree and control
// block construction logic at every call site.
func NewBoardingCollabSpendInfo(tapscript *waddrmgr.Tapscript) (
	*BoardingCollabSpendInfo, error) {

	if tapscript == nil {
		return nil, fmt.Errorf("missing boarding tapscript")
	}

	switch tapscript.Type {
	case waddrmgr.TapscriptTypeFullTree:
		if len(tapscript.Leaves) <= BoardingCollabPathLeafIndex {
			return nil, fmt.Errorf("invalid boarding tapscript leaves")
		}

		collabLeaf := tapscript.Leaves[BoardingCollabPathLeafIndex]
		tapTree := txscript.AssembleTaprootScriptTree(
			tapscript.Leaves...,
		)
		if len(tapTree.LeafMerkleProofs) <= BoardingCollabPathLeafIndex {
			return nil, fmt.Errorf("missing taproot proof for collab leaf")
		}

		controlBlock := tapTree.LeafMerkleProofs[BoardingCollabPathLeafIndex].ToControlBlock(&ARKNUMSKey)
		ctrlBytes, err := controlBlock.ToBytes()
		if err != nil {
			return nil, err
		}

		return &BoardingCollabSpendInfo{
			WitnessScript: collabLeaf.Script,
			ControlBlock:  ctrlBytes,
		}, nil

	case waddrmgr.TapscriptTypePartialReveal:
		if tapscript.ControlBlock == nil ||
			len(tapscript.RevealedScript) == 0 {

			return nil, fmt.Errorf("partial tapscript missing data")
		}

		ctrlBytes, err := tapscript.ControlBlock.ToBytes()
		if err != nil {
			return nil, err
		}

		return &BoardingCollabSpendInfo{
			WitnessScript: tapscript.RevealedScript,
			ControlBlock:  ctrlBytes,
		}, nil

	default:
		return nil, fmt.Errorf("unsupported tapscript type %d",
			tapscript.Type)
	}
}

// NewBoardingTimeoutSpendInfo derives timeout spend information from the given
// tapscript.
func NewBoardingTimeoutSpendInfo(tapscript *waddrmgr.Tapscript) (
	*BoardingTimeoutSpendInfo, error) {

	if tapscript == nil || len(tapscript.Leaves) <= BoardingTimeoutPathLeafIndex {
		return nil, fmt.Errorf("invalid boarding tapscript")
	}

	timeoutLeaf := tapscript.Leaves[BoardingTimeoutPathLeafIndex]
	tapTree := txscript.AssembleTaprootScriptTree(tapscript.Leaves...)
	if len(tapTree.LeafMerkleProofs) <= BoardingTimeoutPathLeafIndex {
		return nil, fmt.Errorf("missing taproot proof for timeout leaf")
	}

	controlBlock := tapTree.LeafMerkleProofs[BoardingTimeoutPathLeafIndex].ToControlBlock(&ARKNUMSKey)
	ctrlBytes, err := controlBlock.ToBytes()
	if err != nil {
		return nil, err
	}

	return &BoardingTimeoutSpendInfo{
		WitnessScript: timeoutLeaf.Script,
		ControlBlock:  ctrlBytes,
	}, nil
}

// BoardingCollabSignDescriptor builds a taproot script spend descriptor for the
// collaborative boarding path.
func BoardingCollabSignDescriptor(keyDesc *keychain.KeyDescriptor,
	amount btcutil.Amount, pkScript []byte, inputIndex int,
	sigHashes *txscript.TxSigHashes, prevFetcher txscript.PrevOutputFetcher,
	spendInfo *BoardingCollabSpendInfo) (*input.SignDescriptor, error) {

	if spendInfo == nil {
		return nil, fmt.Errorf("missing collaborative spend info")
	}

	desc := &input.SignDescriptor{
		SignMethod: input.TaprootScriptSpendSignMethod,
		Output: &wire.TxOut{
			Value:    int64(amount),
			PkScript: pkScript,
		},
		HashType:          txscript.SigHashDefault,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevFetcher,
		InputIndex:        inputIndex,
		WitnessScript:     spendInfo.WitnessScript,
		ControlBlock:      spendInfo.ControlBlock,
	}

	if keyDesc != nil {
		desc.KeyDesc = *keyDesc
	}

	return desc, nil
}

// BoardingTimeoutSignDescriptor builds a taproot timeout script spend descriptor
// for a boarding output.
func BoardingTimeoutSignDescriptor(keyDesc *keychain.KeyDescriptor,
	amount btcutil.Amount, pkScript []byte, inputIndex int,
	sigHashes *txscript.TxSigHashes, prevFetcher txscript.PrevOutputFetcher,
	spendInfo *BoardingTimeoutSpendInfo) (*input.SignDescriptor, error) {

	if spendInfo == nil {
		return nil, fmt.Errorf("missing timeout spend info")
	}

	desc := &input.SignDescriptor{
		SignMethod: input.TaprootScriptSpendSignMethod,
		Output: &wire.TxOut{
			Value:    int64(amount),
			PkScript: pkScript,
		},
		HashType:          txscript.SigHashDefault,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevFetcher,
		InputIndex:        inputIndex,
		WitnessScript:     spendInfo.WitnessScript,
		ControlBlock:      spendInfo.ControlBlock,
	}

	if keyDesc != nil {
		desc.KeyDesc = *keyDesc
	}

	return desc, nil
}

// BoardingCollabWitness constructs the witness stack for the collaborative
// boarding spend path.
func BoardingCollabWitness(serverSig, clientSig input.Signature,
	spendInfo *BoardingCollabSpendInfo) (wire.TxWitness, error) {

	if serverSig == nil || clientSig == nil {
		return nil, fmt.Errorf("missing collaborative signatures")
	}
	if spendInfo == nil {
		return nil, fmt.Errorf("missing collaborative spend info")
	}

	return wire.TxWitness{
		serverSig.Serialize(),
		clientSig.Serialize(),
		spendInfo.WitnessScript,
		spendInfo.ControlBlock,
	}, nil
}

// SignBoardingCollabInput uses the provided signer to produce a Schnorr
// signature for the collaborative boarding tapleaf at the given input index.
func SignBoardingCollabInput(signer input.Signer, tx *wire.MsgTx,
	inputIndex int, spendInfo *BoardingCollabSpendInfo,
	keyDesc *keychain.KeyDescriptor, amount btcutil.Amount, pkScript []byte,
	sigHashes *txscript.TxSigHashes,
	prevFetcher txscript.PrevOutputFetcher) (*schnorr.Signature, error) {

	signDesc, err := BoardingCollabSignDescriptor(
		keyDesc, amount, pkScript, inputIndex, sigHashes, prevFetcher,
		spendInfo,
	)
	if err != nil {
		return nil, err
	}

	rawSig, err := signer.SignOutputRaw(tx, signDesc)
	if err != nil {
		return nil, err
	}

	schnorrSig, ok := rawSig.(*schnorr.Signature)
	if !ok {
		return nil, fmt.Errorf("expected schnorr signature")
	}

	return schnorrSig, nil
}

func BoardingTimoutSpendWitness(signer input.Signer,
	signDesc *input.SignDescriptor, sweepTx *wire.MsgTx) (wire.TxWitness,
	error) {

	// First, we'll generate the sweep signature based on the populated
	// sign desc. This should give us a valid schnorr signature for the
	// timout path leaf.
	timeoutSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// Now that we have the redeem control block, we can construct the
	// final witness needed to spend the script:
	//
	//  <client timout sig> <timout script> <control_block>
	witnessStack := make(wire.TxWitness, 3)
	witnessStack[0] = maybeAppendSighash(timeoutSig, signDesc.HashType)
	witnessStack[1] = signDesc.WitnessScript
	witnessStack[2] = signDesc.ControlBlock

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
