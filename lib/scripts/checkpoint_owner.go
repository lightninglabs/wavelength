package scripts

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
)

// OwnerCheckSigLeaf builds the single-sig owner leaf script for a checkpoint
// taproot tree.
//
// The resulting script is:
//
//	<schnorr_pubkey> OP_CHECKSIG
//
// This leaf is spent by the client alone (no operator co-signature) and is
// used as the collaborative/owner path in the checkpoint output. The operator
// path uses a CSV-delayed unilateral unroll leaf.
func OwnerCheckSigLeaf(pubkey *btcec.PublicKey) ([]byte, error) {
	if pubkey == nil {
		return nil, fmt.Errorf("pubkey must be provided")
	}

	builder := txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(pubkey))
	builder.AddOp(txscript.OP_CHECKSIG)

	return builder.Script()
}
