package lib

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	// CollabMultisigLeafWitnessSize is the estimated witness size for the
	// collaborative spend path of a boarding or vtxo output.
	// The size is calculated as:
	// 1 + 64 bytes for client signature + length byte
	// 1 + 64 bytes for server signature + length byte
	CollabMultisigLeafWitnessSize = lntypes.WeightUnit(1 + 64 + 1 + 64)
)

// UnilateralCSVTimeoutTapLeaf constructs the tap leaf used as the timeout path
// for boarding or VTXO outputs.
//
// The final script used is:
//
//	<timeout_key> OP_CHECKSIG
//	<exit_delay>  OP_CHECKSEQUENCEVERIFY OP_DROP
func UnilateralCSVTimeoutTapLeaf(exitKey *btcec.PublicKey,
	csvDelay uint32) (txscript.TapLeaf, error) {

	builder := txscript.NewScriptBuilder()

	// Ensure the proper party can sign for this output.
	builder.AddData(schnorr.SerializePubKey(exitKey))
	builder.AddOp(txscript.OP_CHECKSIG)

	// Assuming the above passes, then we'll now ensure that the CSV delay
	// has been upheld, dropping the int we pushed on. If the sig above is
	// valid, then a 1 will be left on the stack.
	builder.AddInt64(int64(csvDelay))
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	builder.AddOp(txscript.OP_DROP)

	secondLevelLeafScript, err := builder.Script()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	return txscript.NewBaseTapLeaf(secondLevelLeafScript), nil
}

// MultiSigCollabTapLeaf returns the full tapscript leaf for the collaborative
// multisig script spend path between client and server. This is used for both
// boarding and VTXO outputs.
//
// The final script used is:
//
//	<client_key>   OP_CHECKSIGVERIFY
//	<operator_key> OP_CHECKSIG
func MultiSigCollabTapLeaf(boardingKey,
	operatorKey *btcec.PublicKey) (txscript.TapLeaf, error) {

	builder := txscript.NewScriptBuilder()

	builder.AddData(schnorr.SerializePubKey(boardingKey))
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddData(schnorr.SerializePubKey(operatorKey))
	builder.AddOp(txscript.OP_CHECKSIG)

	timeoutLeafScript, err := builder.Script()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	return txscript.NewBaseTapLeaf(timeoutLeafScript), nil
}

// BuildConnectorOutput constructs a connector output given the number of
// connectors to create, the dust amount per connector and the connector
// address.
func BuildConnectorOutput(numConnectors int, dustAmount btcutil.Amount,
	connectorAddr btcutil.Address) (*wire.TxOut, error) {

	if numConnectors == 0 {
		return nil, fmt.Errorf("num connectors must be > 0")
	}

	// The total amount is just the number of connectors times the dust.
	totalAmount := dustAmount * btcutil.Amount(numConnectors)

	pkScript, err := txscript.PayToAddrScript(connectorAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to create connector script: %w", err)
	}

	return &wire.TxOut{
		Value:    int64(totalAmount),
		PkScript: pkScript,
	}, nil
}

// BuildBatchOutput computes the pk script and output amount of a batch output
// given a set of VTXO leaves, the operator's musig2 signing pub key along with
// the operator's sweep public key and the batch sweep CSV expiry to use in the
// sweep script of the output.
//
// Due to the fact that each transaction in the VTXT includes a zero value
// ephemeral anchor output, we don't need to worry about fees here, and we can
// just add up the amounts of all the leaves to get the final output amount.
//
// The output has two spend paths:
//  1. First, the collaborative key spend path which is a musig2 between all
//     participants in the batch along with the operator.
//  2. Second, the CSV timeout script path which allows the operator to sweep
//     the output after the batch expiry using their solo key.
func BuildBatchOutput(leaves []VTXOLeaf, operatorMuSigKey,
	sweepPub *btcec.PublicKey, sweepDelay uint32) (*wire.TxOut, error) {

	if len(leaves) == 0 {
		return nil, fmt.Errorf("batch output requires at least one " +
			"vtxo leaf")
	}

	// Compute the sweep tap leaf. This will be applied as a tweak to all
	// the batch outputs along with all the (non-leaf) VTX outputs.
	sweepTapLeaf, err := UnilateralCSVTimeoutTapLeaf(sweepPub, sweepDelay)
	if err != nil {
		return nil, err
	}

	// We'll collect the set of unique signers that will participate in the
	// VTXT signing.
	var (
		signers     = make([]*btcec.PublicKey, 0)
		seenSigners = make(map[string]struct{})
	)
	addSigner := func(key *btcec.PublicKey) {
		keyStr := string(schnorr.SerializePubKey(key))
		if _, ok := seenSigners[keyStr]; ok {
			return
		}
		seenSigners[keyStr] = struct{}{}
		signers = append(signers, key)
	}

	// Add the operator's musig2 key as a signer.
	addSigner(operatorMuSigKey)

	// Since all the transactions in the VTXT include a zero value ephemeral
	// anchor output, we can just sum up the amounts of all the leaves to
	// get the final output amount. We also collect the signer keys here.
	var totalAmount btcutil.Amount
	for _, leaf := range leaves {
		totalAmount += leaf.Amount
		addSigner(leaf.SigningKey)
	}

	// We can now compute the final output key by doing a musig2 aggregation
	// of the co-signers and then tweaking the key with the sweep tap leaf.
	key, _, _, err := musig2.AggregateKeys(
		signers, true, musig2.WithTaprootKeyTweak(sweepTapLeaf.Script),
	)
	if err != nil {
		return nil, err
	}

	scriptPubkey, err := txscript.PayToTaprootScript(key.FinalKey)
	if err != nil {
		return nil, err
	}

	return &wire.TxOut{
		Value:    int64(totalAmount),
		PkScript: scriptPubkey,
	}, nil
}

const (
	// VTXOCollabPathLeafIndex is the index of the collaborative multisig
	// path.
	VTXOCollabPathLeafIndex = 0

	// VTXOTimeoutPathLeafIndex is the index of the CSV timeout path.
	VTXOTimeoutPathLeafIndex = 1
)

// VTXOTapScript constructs the full tapscript for a VTXO output. The
// leaves of the tapscript are:
// - Collaborative multisig spend path between client and server.
// - CSV Timeout path allowing client to recover funds after exit delay.
func VTXOTapScript(clientKey, serverKey *btcec.PublicKey,
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
