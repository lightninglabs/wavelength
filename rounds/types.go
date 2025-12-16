package rounds

import (
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/clientconn"
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
