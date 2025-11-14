package lib

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/ark/lib/types"
	"github.com/lightningnetwork/lnd/input"
)

// Explorer provides an API for clients to fetch transaction data needed for
// signing
type Explorer interface {
	// GetPrevOutputs returns the previous outputs for all inputs in a
	// transaction.
	GetPrevOutputs(tx *wire.MsgTx) (map[wire.OutPoint]*wire.TxOut, error)
}

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

type ParticipantRoundRequest struct {
	VTXOReqs     []*VTXORequest
	BoardingReqs []*BoardingRequest
	LeaveReqs    []*LeaveRequest
	ForfeitReqs  []*ForfeitRequest
}

// LeaveRequest represents a request to leave the Ark with an on-chain UTXO.
type LeaveRequest struct {
	// Output is the output that will be created to return funds to the
	// client when leaving the Ark.
	Output *wire.TxOut
}

// ForfeitTxSig represents an unsigned forfeit transaction with the client's VTXO signature
type ForfeitTxSig struct {
	// UnsignedTx is the forfeit transaction without any witness data
	UnsignedTx *wire.MsgTx

	// ClientVTXOSig is the client's schnorr signature for the VTXO input
	ClientVTXOSig *schnorr.Signature
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

	// SigningKey is the public key that the client will use in the building
	// of the VTXO tree during Musig2 signing sessions.
	SigningKey *btcec.PublicKey
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

type BoardingInputSignature struct {
	// InputIndex is the index of the input in the transaction
	InputIndex int

	// Outpoint identifies which boarding input this signature is for
	Outpoint wire.OutPoint

	// ClientSignature is the client's schnorr signature
	ClientSignature *schnorr.Signature
}

type ArkClient interface {
	ListVTXOs() ([]*ClientVTXO, error)
	CreateForfeitRequest(vtxo *wire.OutPoint) (*ForfeitRequest,
		error)

	// NewBoardingAddress derives a new boarding address. It uses the
	// operator information to construct the proper boarding script. It
	// registers the script with the backing wallet so that it can properly
	// track and spend outputs sent to the address.
	NewBoardingAddress(terms *OperatorTerms) (btcutil.Address, error)

	// ListBoardingUTXOs lists all known boarding UTXOs in the wallet.
	ListBoardingUTXOs() ([]*types.BoardingUTXO, error)

	// CreateBoardingRequest creates a boarding request using a UTXO that
	// funds the given boarding address.
	CreateBoardingRequest(boardingAddress *types.BoardingUTXO) (
		*BoardingRequest, error)

	// CreateLeaveRequest creates a leave request that returns the given
	// amount back to the client's wallet.
	CreateLeaveRequest(amt btcutil.Amount) (*LeaveRequest, error)

	// CreateVTXORequest creates a VTXO request that locks the given
	// amount in a VTXO output. It uses the provided operator terms to
	// construct the proper VTXO script.
	CreateVTXORequest(terms *OperatorTerms, amt btcutil.Amount) (*VTXORequest, error)

	// SweepExpiredBoardingUTXOs creates a transaction that sweeps all
	// expired boarding UTXOs back to the wallet.
	SweepExpiredBoardingUTXOs() (*wire.MsgTx, error)

	// SignBoardingInputs signs all boarding inputs in the given
	// transaction. It returns the signatures for each boarding input.
	SignBoardingInputs(uuid string, tx *wire.MsgTx) ([]*BoardingInputSignature, error)

	BatchJoined(uuid string, boardReqs []*BoardingRequest,
		vtxoReqs []*VTXORequest, leaveReqs []*LeaveRequest,
		forfeitReqs []*ForfeitRequest) error

	BatchCreated(uuid string, clientInfo *ClientBatchInfo) error
	GetNoncesForSigner(uuid string, key *btcec.PublicKey) (map[string]Musig2PubNonce, error)

	SubmitNonces(uuid string, signingKey *btcec.PublicKey,
		nonces map[string][]Musig2PubNonce) (
		map[string]*musig2.PartialSignature, error)

	SubmitTreeSigs(uuid string, signingKey *btcec.PublicKey,
		sigs map[string]*schnorr.Signature) error

	GetSignedForfeits(uuid string, serverForfeitScript []byte) ([]*ForfeitTxSig, error)
}

// VTXOLeaf represents the minimal information needed for batch output construction
type VTXOLeaf struct {
	// Amount is the amount of satoshis in this VTXO
	Amount btcutil.Amount

	// SigningKey is the public key that will be used in Musig2 signing sessions
	SigningKey *btcec.PublicKey
}

type ConnectorOutputInfo struct {
	// Idx is the index of this connector output in the batch transaction.
	Idx int

	NumLeaves    int
	ConnectorKey *btcec.PublicKey

	// Tree is the connector VTX tree for this connector output.
	Tree *Tree
}

type BatchOutputInfo struct {
	// Idx is the index of this batch output in the batch transaction.
	Idx int

	// SignerKey is they key that the operator will use for the Musig2
	// signing sessions for this batch output.
	SignerKey *btcec.PublicKey

	// Tree is the VTXO tree for this batch output.
	// Tree contains SweepKey, SweepDelay, and PrevOut.
	Tree *Tree
}

// ClientBatchInfo contains client-specific batch information
type ClientBatchInfo struct {
	// Always present
	Transaction *wire.MsgTx

	// Optional fields based on request types
	VTXOInfo      *ClientVTXOInfo      // If client has VTXO requests
	ConnectorInfo *ClientConnectorInfo // If client has forfeit requests
}

// ClientVTXOInfo contains VTXO-specific information for a client
type ClientVTXOInfo struct {
	BatchOutput *BatchOutputInfo // The batch output containing VTXOs
	VTXOPaths   []*Tree          // Path to each VTXO requested by client
}

// ClientConnectorInfo contains connector-specific information for a client
type ClientConnectorInfo struct {
	ConnectorOutput *ConnectorOutputInfo // The connector output
	ConnectorPaths  []*Tree              // Path to each forfeit connector (by index)
}

type ArkServer interface {
	// Terms returns the various terms of the operator that the client
	// must obey when making requests.
	Terms() (*OperatorTerms, error)

	// StartNewBatch creates a new batch and returns its UUID
	StartNewBatch() (string, error)

	// Batch management methods that delegate to the appropriate batch
	RegisterRequests(batchUUID string,
		req *ParticipantRoundRequest) (string, error)

	SealBatch(batchUUID string) error

	GetClientBatchInfo(batchUUID string, clientRequestID string) (*ClientBatchInfo, error)

	RegisterNonces(batchUUID string, signer *btcec.PublicKey,
		nonces map[string]Musig2PubNonce) (bool, error)

	GetAggNonce(batchUUID string) (map[string][]Musig2PubNonce, error)

	AddPartialSignatures(batchUUID string, signer *btcec.PublicKey,
		sigs map[string]*musig2.PartialSignature) error

	GetTreeSigs(batchUUID string) (map[string]*schnorr.Signature, error)

	AddBoardingSignatures(batchUUID string, sigs []*BoardingInputSignature) error

	SubmitSignedForfeits(batchUUID string, clientID string, forfeitTxSigs []*ForfeitTxSig) error

	SignInputs(batchUUID string) (*wire.MsgTx, error)
}

type BatchBuilder interface {
	UUID() string
	RegisterRequests(boardingReqs []*BoardingInput,
		leaveReqs []*LeaveRequest, vtxoReqs []*VTXORequest,
		forfeitReqs []*ForfeitRequest) (string, error)

	SealBatch() error

	GetClientBatchInfo(clientRequestID string) (*ClientBatchInfo, error)

	RegisterNonces(signer *btcec.PublicKey,
		nonces map[string]Musig2PubNonce) (bool, error)

	GetAggNonce() (map[string][]Musig2PubNonce, error)

	AddPartialSignatures(signer *btcec.PublicKey,
		sigs map[string]*musig2.PartialSignature) error

	GetTreeSigs() (map[string]*schnorr.Signature, error)

	AddBoardingSignatures(sigs []*BoardingInputSignature) error

	// Forfeit transaction handling
	SubmitSignedForfeits(clientID string, forfeitTxSigs []*ForfeitTxSig) error

	SignInputs() (*wire.MsgTx, error)

	// GetAllVTXORequests returns all VTXO requests in this batch
	GetAllVTXORequests() []*VTXORequest

	// GetBatchOutputs returns all batch outputs containing VTXO trees
	GetBatchOutputs() []*BatchOutputInfo

	// GetAllForfeitRequests returns all forfeit requests in this batch
	GetAllForfeitRequests() []*ForfeitRequest
}

func NewVTXORequest(
	operatorCollabPub,
	clientCollabPub,
	clientSignerPub *btcec.PublicKey,
	exitCSVDelay uint32,
	amount btcutil.Amount,
) (*VTXORequest, error) {

	// Compute the tapscript for the VTXO output.
	tapscript, err := VTXOTapScript(
		clientCollabPub, operatorCollabPub, exitCSVDelay,
	)
	if err != nil {
		return nil, err
	}

	outputKey, err := tapscript.TaprootKey()
	if err != nil {
		return nil, err
	}

	pkscript, err := input.PayToTaprootScript(outputKey)
	if err != nil {
		return nil, err
	}

	return &VTXORequest{
		Amount:      amount,
		PkScript:    pkscript,
		Expiry:      exitCSVDelay,
		ClientKey:   clientCollabPub,
		OperatorKey: operatorCollabPub,
		SigningKey:  clientSignerPub,
	}, nil
}

func LeavesFromVTXOReqs(reqs []*VTXORequest) []Leaf {
	leaves := make([]Leaf, len(reqs))
	for i, req := range reqs {
		leaves[i] = Leaf{
			PkScript:  req.PkScript,
			Amount:    int64(req.Amount),
			SignerKey: req.SigningKey,
		}

	}
	return leaves
}

func VTXOLeavesFromRequests(reqs []*VTXORequest) []VTXOLeaf {
	leaves := make([]VTXOLeaf, len(reqs))
	for i, req := range reqs {
		leaves[i] = VTXOLeaf{
			Amount:     req.Amount,
			SigningKey: req.SigningKey,
		}
	}
	return leaves
}
