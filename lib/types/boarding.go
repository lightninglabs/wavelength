package types

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightningnetwork/lnd/keychain"
)

// OperatorTerms holds the information that the operator will share with
// clients. It communicates the server's terms to the client.
type OperatorTerms struct {
	// PubKey is the operator's main public key. This should be used for
	// constructing boarding scripts.
	PubKey *btcec.PublicKey

	// BoardingExitDelay is the minimum CSV delay to use for boarding
	// outputs that the operator expects.
	BoardingExitDelay uint32

	// VTXOExitDelay is the minimum CSV delay to use for VTXO outputs. This
	// delay will give the server time to respond to unilateral spends of
	// a VTXO that has been forfeit or spent.
	VTXOExitDelay uint32
}

// JoinRoundRequest represents a participant's request to join a round.
type JoinRoundRequest struct {
	// Identifier is the participant's public key identifier associated with
	// this request.
	Identifier *btcec.PublicKey

	// VTXOReqs specifies the new VTXOs the client wants to receive.
	VTXOReqs []*VTXORequest

	// BoardingReqs specifies the boarding UTXOs the client wants to use
	// to board the Ark.
	BoardingReqs []*BoardingRequest

	// LeaveReqs specifies the requests to leave the Ark with on-chain
	// UTXOs.
	LeaveReqs []*LeaveRequest

	// ForfeitReqs specifies the requests to forfeit VTXOs.
	ForfeitReqs []*ForfeitRequest
}

// LeaveRequest represents a request to leave the Ark with an on-chain UTXO.
type LeaveRequest struct {
	// Output is the output that will be created to return funds to the
	// client when leaving the Ark.
	Output *wire.TxOut
}

// ForfeitRequest represents a request to forfeit a VTXO.
type ForfeitRequest struct {
	// VTXOOutpoint is the outpoint of the VTXO to forfeit.
	VTXOOutpoint *wire.OutPoint
}

type VTXORequest struct {
	// Amount is the amount of satoshis to lock in the VTXO.
	Amount btcutil.Amount

	// PkScript is the output script of the VTXO. This will have
	// both a collaborative and unilateral spend path.
	PkScript []byte

	// Expiry is the CSV delay used in the unilateral timeout script path
	// of the VTXO.
	Expiry uint32

	// ClientKey is the public key of the client used in the construction
	// of the collaborative spend path of the VTXO.
	ClientKey *btcec.PublicKey

	// OperatorKey is the public key of the operator used in the
	// construction of the collaborative spend path of the VTXO.
	OperatorKey *btcec.PublicKey

	// SigningKey is the key descriptor that the client will use in the
	// building of the VTXO tree during Musig2 signing sessions. We use
	// keychain.KeyDescriptor instead of just *btcec.PublicKey because we
	// need the key locator for signing operations.
	SigningKey keychain.KeyDescriptor
}

// BoardingRequest represents a request to board the Ark via a UTXO.
type BoardingRequest struct {
	// Outpoint represents the UTXO that will be used as input to the batch
	// transaction.
	Outpoint *wire.OutPoint

	// ClientKey is the public key used for the client in the boarding
	// tapscripts.
	ClientKey *btcec.PublicKey

	// OperatorKey is the public key used for the operator in the boarding
	// tapscript collaborative spend path.
	OperatorKey *btcec.PublicKey

	// ExitDelay is the CSV delay used in the unilateral timeout script
	// path of the boarding output. This must be at least the operator's
	// minimum boarding exit delay.
	ExitDelay uint32
}

// BoardingInputSignature represents the client's signature for a boarding
// input in the batch transaction.
type BoardingInputSignature struct {
	// InputIndex is the index of the input in the transaction
	InputIndex int

	// Outpoint identifies which boarding input this signature is for
	Outpoint wire.OutPoint

	// ClientSignature is the client's schnorr signature
	ClientSignature *schnorr.Signature
}

// ForfeitTxSig represents an unsigned forfeit transaction with the client's
// VTXO signature.
type ForfeitTxSig struct {
	// UnsignedTx is the forfeit transaction without any witness data
	UnsignedTx *wire.MsgTx

	// ClientVTXOSig is the client's schnorr signature for the VTXO input
	ClientVTXOSig *schnorr.Signature
}

// ConnectorOutputInfo contains the information about a connector output
// in the batch transaction. A batch transaction can have multiple connector
// outputs, each with its own connector VTX tree.
type ConnectorOutputInfo struct {
	// Idx is the index of this connector output in the batch transaction.
	Idx int

	// NumLeaves is the number of leaves in the connector VTX tree for this
	// connector output.
	NumLeaves int

	// ConnectorKey is the key that the operator will use as its key for
	// each output in the connector VTX tree.
	ConnectorKey *btcec.PublicKey

	// Tree is the connector VTX tree for this connector output.
	Tree *tree.Tree
}

// BatchOutputInfo contains the information about a batch output in the
// batch transaction. A batch transaction can have multiple batch outputs,
// each with its own VTXO tree.
type BatchOutputInfo struct {
	// Idx is the index of this batch output in the batch transaction.
	Idx int

	// SignerKey is they key that the operator will use for the Musig2
	// signing sessions for this batch output.
	SignerKey *btcec.PublicKey

	// Tree is the VTXO tree for this batch output.
	// Tree contains SweepKey, SweepDelay, and PrevOut.
	Tree *tree.Tree
}

// ClientBatchInfo contains batch information specific to a client. It contains
// all the info the client needs in order to validate that their requests were
// included correctly in the batch transaction.
//   - any boarding request will have a corresponding boarding input in the
//     batch transaction.
//   - any VTXO request will have a corresponding output in the batch
//     transaction.
//   - any forfeit request will have a corresponding connector leaf.
//   - any leave request will have a corresponding output in the batch
//     transaction.
type ClientBatchInfo struct {
	// Transaction is the batch transaction.
	Transaction *wire.MsgTx

	// BatchOutputs contains the batch output info for each batch output
	// that is relevant to the client. The number of VTXO leaves should
	// match the number of VTXO requests made by the client.
	BatchOutputs []*BatchOutputInfo

	// ConnectorOutputs contains the connector output info for each
	// connector output that is relevant to the client. The number of
	// connector leaves should match the number of connector requests.
	ConnectorOutputs []*ConnectorOutputInfo
}
