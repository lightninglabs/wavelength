package rounds

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/routing/route"
)

// ClientID is a type alias for clientconn.ClientID to improve readability
// within this package.
type ClientID = clientconn.ClientID

// SigningKeyHex is the serialized compressed public key used as a unique
// identifier for VTXO signing keys in a batch.
type SigningKeyHex = route.Vertex

// TxID is an alias for tree.TxID (chainhash.Hash), used as a key in maps for
// efficient lookups.
type TxID = tree.TxID

// BoardingInput represents a validated boarding input that will be spent in
// the batch transaction. It contains all the data needed to construct the
// input and sign it.
type BoardingInput struct {
	// Outpoint represents the UTXO that will be used as input to the batch
	// transaction.
	Outpoint *wire.OutPoint

	// Tapscript contains the boarding tapscript for spending via script
	// path.
	Tapscript *waddrmgr.Tapscript

	// Value is the amount of satoshis in this UTXO.
	Value btcutil.Amount

	// PkScript is the script of the UTXO (taproot script).
	PkScript []byte

	// ClientKey is the public key of the client who owns this boarding
	// input.
	ClientKey *btcec.PublicKey

	// OperatorKeyDesc is the key descriptor of the operator's key
	// that corresponds to the operator key in the tapscript.
	OperatorKeyDesc *keychain.KeyDescriptor
}
