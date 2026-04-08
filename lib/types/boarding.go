package types

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/taproot-assets/proof"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

// VTXOOwnerKeyFamily is the key family used for long-lived VTXO owner keys.
// Owner keys are committed into the VTXO policy and must remain stable across
// refreshes. This is distinct from the MuSig2 tree-signing key family used
// during round construction.
const VTXOOwnerKeyFamily keychain.KeyFamily = 44

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

	// ForfeitScript is the output script that clients must use for the
	// penalty output in forfeit transactions. This allows the server to
	// claim forfeited funds.
	ForfeitScript []byte

	// SweepKey is the operator key used in VTXT sweep paths.
	SweepKey *btcec.PublicKey

	// SweepDelay is the batch-wide absolute timelock (blocks).
	SweepDelay uint32

	// DustLimit enforces minimum output value for boarding/funding flows.
	DustLimit btcutil.Amount

	// MinBoardingAmount is the minimum amount clients must contribute.
	MinBoardingAmount btcutil.Amount

	// MaxBoardingAmount caps the amount accepted per request (optional).
	MaxBoardingAmount btcutil.Amount

	// FeeRate reflects the operator's target package feerate (sat/vByte).
	FeeRate btcutil.Amount

	// MinOperatorFee is the minimum fee (satoshis) the operator
	// requires per join request. The fee is the difference between
	// total input value and total output value.
	MinOperatorFee btcutil.Amount

	// MinConfirmations is the minimum confs required on boarding inputs.
	MinConfirmations uint32
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

	// Auth contains the BIP-322 payload that authorizes this join
	// request.
	Auth *JoinRoundAuth
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

	// Amount is the local value of the forfeited VTXO in satoshis. This
	// is used by the client when validating a round before registration.
	// It is not part of the join-round wire encoding, where the outpoint
	// remains the source of truth.
	Amount btcutil.Amount

	// AuthSpend is the unilateral proof/auth spend path used locally for
	// join-auth when settling a custom-script output into a round. This is
	// local-only metadata and is not serialized onto the join-round wire.
	AuthSpend *arkscript.SpendPath

	// ForfeitSpend is the operator-backed spend path used locally
	// to build the actual round forfeit transaction for a
	// custom-script output. This is local-only metadata and is
	// not serialized onto the join-round wire.
	ForfeitSpend *arkscript.SpendPath
}

// VTXORequest describes a requested round output. The policy template is the
// authoritative join-round representation, while local owner metadata is kept
// only when this client controls the resulting VTXO.
type VTXORequest struct {
	// Amount is the amount of satoshis to lock in the VTXO.
	Amount btcutil.Amount

	// PolicyTemplate is the semantic arkscript policy for the requested
	// output. This is the authoritative join-round representation.
	PolicyTemplate []byte

	// PkScript is the output script of the VTXO. This will have
	// both a collaborative and unilateral spend path.
	PkScript []byte

	// Expiry is the CSV delay used in the unilateral timeout script path
	// of the VTXO.
	Expiry uint32

	// ClientKey is the public key of the client used in the construction
	// of the collaborative spend path of the VTXO.
	ClientKey *btcec.PublicKey

	// OwnerKey is the local key descriptor for the VTXO owner when this
	// client controls the resulting output. This is local-only
	// metadata and is not serialized onto the join-round wire.
	OwnerKey keychain.KeyDescriptor
	// OperatorKey is the public key of the operator used in the
	// construction of the collaborative spend path of the VTXO.
	OperatorKey *btcec.PublicKey

	// SigningKey is the key descriptor that the client will use in the
	// building of the VTXO tree during Musig2 signing sessions. We use
	// keychain.KeyDescriptor instead of just *btcec.PublicKey because we
	// need the key locator for signing operations.
	SigningKey keychain.KeyDescriptor
}

// HasLocalOwner reports whether the request carries a local owner descriptor
// that should be preserved through confirmation and persistence. A nil owner
// pubkey, not a zero-valued key locator, is the sentinel for foreign outputs.
func (r *VTXORequest) HasLocalOwner() bool {
	return r != nil && r.OwnerKey.PubKey != nil
}

// BoardingRequest represents a request to board the Ark via a UTXO.
type BoardingRequest struct {
	// Outpoint represents the UTXO that will be used as input to the batch
	// transaction.
	Outpoint *wire.OutPoint

	// PolicyTemplate is the semantic arkscript policy for the boarding
	// output. This is the authoritative join-round representation.
	PolicyTemplate []byte

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

	// TxProof is the SPV proof that the boarding UTXO exists in a
	// confirmed block. This allows the server to verify the UTXO without
	// querying its own chain source. The proof includes the transaction,
	// block header, merkle proof, and the taproot output construction
	// details (internal key and merkle root). None if the server will
	// verify via its own chain source.
	TxProof fn.Option[proof.TxProof]
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

	// SpendPath is the canonical arkscript spend path for the
	// forfeited VTXO input. This makes the custom or standard
	// tapscript leaf an explicit part of round messaging instead
	// of implicit witness metadata.
	SpendPath *arkscript.SpendPath
}

// ConnectorLeafInfo contains information about a connector leaf assigned to a
// specific forfeit request.
type ConnectorLeafInfo struct {
	// LeafOutpoint is the outpoint of the connector leaf that the forfeit
	// transaction should spend. This is the actual outpoint from the leaf
	// transaction in the connector tree.
	LeafOutpoint wire.OutPoint

	// LeafOutput is the transaction output for the connector leaf. This
	// contains the value and pkScript needed to construct the forfeit
	// transaction witness.
	LeafOutput *wire.TxOut
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

	// ConnectorLeafMap maps each forfeited VTXO outpoint to its assigned
	// connector leaf information. This allows the client to determine which
	// connector leaf corresponds to each of their forfeit requests.
	ConnectorLeafMap map[wire.OutPoint]*ConnectorLeafInfo
}
