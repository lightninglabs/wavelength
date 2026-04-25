package rounds

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
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

// InputSigsMap maps client IDs to their boarding input signatures.
// This type alias improves readability in struct definitions.
type InputSigsMap = map[ClientID][]*types.BoardingInputSignature

// ForfeitTxsMap maps client IDs to their submitted forfeit transactions.
type ForfeitTxsMap = map[ClientID][]*types.ForfeitTxSig

// ConnectorTreeDescriptor captures the information needed to reconstruct a
// connector tree and its output placement in the commitment transaction.
type ConnectorTreeDescriptor struct {
	// OutputIndex is the connector output index in the commitment
	// transaction.
	OutputIndex int

	// NumLeaves is the number of connector leaves for this output.
	NumLeaves int

	// ForfeitScript is the penalty output script for forfeit transactions.
	ForfeitScript []byte
}

// ForfeitInfo records how a VTXO was forfeited in a round.
type ForfeitInfo struct {
	// RoundID is the round in which the VTXO was forfeited.
	RoundID RoundID

	// ConnectorOutputIndex is the connector output index in the
	// commitment transaction.
	ConnectorOutputIndex int

	// LeafIndex is the leaf index within the connector tree.
	LeafIndex int

	// ForfeitTx is the completed forfeit transaction.
	ForfeitTx *wire.MsgTx
}

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

// ForfeitInput represents a validated forfeit input from a VTXO that will be
// spent in the batch transaction. The client is forfeiting their VTXO back to
// the operator.
type ForfeitInput struct {
	// Outpoint is the virtual outpoint identifying the VTXO being
	// forfeited.
	Outpoint *wire.OutPoint

	// VTXO is the VTXO being forfeited, retrieved from the VTXOStore.
	VTXO *VTXO
}

// ConnectorLeafAssignment binds a forfeit input to a specific connector leaf.
type ConnectorLeafAssignment struct {
	// ConnectorOutputIndex is the index of the connector output in the
	// commitment transaction.
	ConnectorOutputIndex int

	// LeafIndex is the index of the leaf within the connector tree.
	LeafIndex int

	// LeafOutpoint is the outpoint for the connector leaf output.
	LeafOutpoint wire.OutPoint

	// LeafOutput is the transaction output for the connector leaf.
	LeafOutput *wire.TxOut
}

// ClientRegistration holds all validated data for a client's join request.
// This is created after validation succeeds and stored in the FSM state.
type ClientRegistration struct {
	// ClientID is the unique identifier for this client.
	ClientID ClientID

	// BoardingInputs are the boarding UTXOs this client is contributing.
	BoardingInputs []*BoardingInput

	// LeaveOutputs are the leave request outputs for this client. Under
	// the #270 seal-time fee handshake, the Value on each TxOut is the
	// client's intent target_amount_sat (pre-fee). The seal-time fee
	// builder mutates the designated change entry to its residual amount
	// before the PSBT is built.
	LeaveOutputs []*wire.TxOut

	// VTXODescriptors maps signing key hex strings to their VTXO
	// descriptors. Each VTXO request has a unique signing key. The
	// Amount field carries the target (pre-fee) amount under seal-time;
	// the builder rewrites the designated change entry's amount to the
	// residual at seal time.
	VTXODescriptors map[SigningKeyHex]*tree.VTXODescriptor

	// ForfeitInputs are the VTXOs this client is forfeiting back to the
	// operator.
	ForfeitInputs []*ForfeitInput

	// IntentVTXOReqs preserves the original per-request metadata from
	// the intent — specifically IsChange markers and the positional
	// order the quote's VTXOQuote slice must echo back. The quote
	// builder iterates this slice to locate the designated change
	// output and to stamp residual amounts in the same order the
	// client submitted.
	IntentVTXOReqs []*types.VTXORequest

	// IntentLeaveReqs is the leave-output analogue of IntentVTXOReqs.
	// Preserves IsChange markers and order; LeaveOutputs[i] always
	// corresponds to IntentLeaveReqs[i].
	IntentLeaveReqs []*types.LeaveRequest
}
