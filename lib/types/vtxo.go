package types

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
)

// ServerVTXO represents a VTXO as stored by the server/operator
type ServerVTXO struct {
	// Outpoint is the outpoint of the VTXO in the batch transaction
	Outpoint *wire.OutPoint

	// Amount is the amount of satoshis in the VTXO
	Amount btcutil.Amount

	// ClientKey is the client's collaborative key used in the VTXO script
	ClientKey *btcec.PublicKey

	// OperatorKey is the operator's collaborative key used in the VTXO script
	OperatorKey *btcec.PublicKey

	// Expiry is the CSV delay for the unilateral timeout path
	Expiry uint32

	// OriginalRequest is the original VTXO request, needed for witness reconstruction
	// We store this as an interface{} to avoid circular import
	OriginalRequest interface{}
}
