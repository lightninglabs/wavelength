package round

import (
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/roundwire"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"google.golang.org/protobuf/proto"
)

// JoinRoundRequest is sent from client to server to request joining a round.
// This implements ClientEvent and is emitted via Outbox.
type JoinRoundRequest struct {
	actor.BaseMessage

	// Identifier is the participant key used for the join-auth
	// challenge script and input-0 signature.
	Identifier *btcec.PublicKey

	// BoardingRequests contains all boarding UTXO details for this
	// session. Each confirmed intent contributes exactly one boarding
	// request so the server can register them in a single batch.
	BoardingRequests []types.BoardingRequest

	// VTXORequests specifies the VTXOs the client wants to receive.
	VTXORequests []types.VTXORequest

	// ForfeitRequests specifies the VTXOs the client wants to forfeit.
	ForfeitRequests []*ForfeitRequest

	// LeaveRequests contains VTXOs being exited to on-chain outputs. Each
	// leave request specifies only the on-chain destination output. The
	// server includes these in the batch transaction; any forfeited VTXOs
	// are listed separately in ForfeitRequests.
	LeaveRequests []*LeaveRequest

	// RoundID is optional; when empty it instructs the server to assign
	// a new round. When non-empty, the request is for the specified round.
	RoundID string

	// Auth contains the BIP-322 authorization payload for this
	// request. Nil when join request auth is disabled (tests).
	Auth *types.JoinRoundAuth
}

// ForfeitRequest describes a VTXO that will be forfeited in the round.
type ForfeitRequest struct {
	// VTXOOutpoint identifies the VTXO to forfeit.
	VTXOOutpoint wire.OutPoint
}

// LeaveRequest describes a leave output to be included in the batch
// transaction. This represents a client exiting the Ark by forfeiting an
// existing VTXO and receiving an on-chain output instead of a new VTXO.
type LeaveRequest struct {
	// Output is the on-chain destination output. Contains the value and
	// pkScript for the leave output that will be included in the batch tx.
	Output *wire.TxOut
}

func (m *JoinRoundRequest) clientOutMsgSealed() {}

// SubmitNoncesRequest is sent from client to server with MuSig2 nonces.
// This implements ClientOutMsg and is emitted via Outbox.
type SubmitNoncesRequest struct {
	actor.BaseMessage

	// RoundID identifies the round.
	RoundID RoundID

	// Nonces maps signing keys to their per-transaction MuSig2 public
	// nonces. The outer map is keyed by signing key (one per VTXO), and
	// the inner map is keyed by transaction ID. This structure matches
	// the server's expected format where nonces are grouped by cosigner.
	Nonces map[SignerKey]map[tree.TxID]tree.Musig2PubNonce
}

func (m *SubmitNoncesRequest) clientOutMsgSealed() {}

// SubmitPartialSigRequest is sent from client to server with partial
// signatures. This implements ClientEvent and is emitted via Outbox.
type SubmitPartialSigRequest struct {
	actor.BaseMessage

	// RoundID identifies the round.
	RoundID RoundID

	// Signatures maps signing keys to their per-transaction MuSig2 partial
	// signatures. The outer map is keyed by signing key (one per VTXO), and
	// the inner map is keyed by transaction ID. This structure matches the
	// server's expected format where signatures are grouped by cosigner.
	Signatures map[SignerKey]map[tree.TxID]*musig2.PartialSignature
}

func (m *SubmitPartialSigRequest) clientOutMsgSealed() {}

// SubmitForfeitSigRequest is sent from client to server with the boarding input
// signature. This implements ClientEvent and is emitted via Outbox.
type SubmitForfeitSigRequest struct {
	actor.BaseMessage

	// RoundID identifies the round.
	RoundID RoundID

	// Signatures contains structured boarding input signatures. Each
	// signature includes the input index, outpoint, and schnorr signature
	// for the collaborative tapscript spend path.
	Signatures []*types.BoardingInputSignature
}

func (m *SubmitForfeitSigRequest) clientOutMsgSealed() {}

// RPCService returns the mailbox RPC service for this message.
func (m *JoinRoundRequest) RPCService() string {
	return roundwire.ServiceName
}

// RPCMethod returns the mailbox RPC method for this message.
func (m *JoinRoundRequest) RPCMethod() string {
	return roundwire.MethodJoinRoundRequest
}

// ToProto converts JoinRoundRequest to a protobuf payload message.
func (m *JoinRoundRequest) ToProto() proto.Message {
	payload, err := joinRoundRequestPayload(m)
	if err != nil {
		log.ErrorS(nil, "Encode JoinRoundRequest failed", err)

		return nil
	}

	msg, err := roundwire.WrapPayload(payload)
	if err != nil {
		log.ErrorS(nil, "Wrap JoinRoundRequest payload failed", err)

		return nil
	}

	return msg
}

// RPCService returns the mailbox RPC service for this message.
func (m *SubmitNoncesRequest) RPCService() string {
	return roundwire.ServiceName
}

// RPCMethod returns the mailbox RPC method for this message.
func (m *SubmitNoncesRequest) RPCMethod() string {
	return roundwire.MethodSubmitNoncesRequest
}

// ToProto converts SubmitNoncesRequest to a protobuf payload message.
func (m *SubmitNoncesRequest) ToProto() proto.Message {
	payload := &roundwire.SubmitNoncesPayload{
		RoundId: m.RoundID.String(),
		Entries: make(
			[]*roundwire.SignerNonceBundle, 0,
			len(m.Nonces),
		),
	}

	for signerKey, txNonces := range m.Nonces {
		entry := &roundwire.SignerNonceBundle{
			SignerKeyHex: hex.EncodeToString(signerKey[:]),
			Nonces: make(
				[]*roundwire.TxNonceEntry, 0, len(txNonces),
			),
		}

		for txID, nonce := range txNonces {
			entry.Nonces = append(entry.Nonces,
				&roundwire.TxNonceEntry{
					TxIdHex:  txID.String(),
					NonceHex: roundwire.EncodeNonce(nonce),
				},
			)
		}

		roundwire.SortTxNonceEntries(entry.Nonces)
		payload.Entries = append(payload.Entries, entry)
	}

	roundwire.SortSignerNonceBundles(payload.Entries)

	msg, err := roundwire.WrapPayload(payload)
	if err != nil {
		log.ErrorS(nil, "Wrap SubmitNoncesRequest payload failed", err)

		return nil
	}

	return msg
}

// RPCService returns the mailbox RPC service for this message.
func (m *SubmitPartialSigRequest) RPCService() string {
	return roundwire.ServiceName
}

// RPCMethod returns the mailbox RPC method for this message.
func (m *SubmitPartialSigRequest) RPCMethod() string {
	return roundwire.MethodSubmitPartialSigRequest
}

// ToProto converts SubmitPartialSigRequest to a protobuf payload message.
func (m *SubmitPartialSigRequest) ToProto() proto.Message {
	payload := &roundwire.SubmitPartialSigsPayload{
		RoundId: m.RoundID.String(),
		Entries: make(
			[]*roundwire.SignerSigBundle, 0,
			len(m.Signatures),
		),
	}

	for signerKey, txSigs := range m.Signatures {
		entry := &roundwire.SignerSigBundle{
			SignerKeyHex: hex.EncodeToString(signerKey[:]),
			Signatures: make(
				[]*roundwire.TxSigEntry, 0, len(txSigs),
			),
		}

		for txID, sig := range txSigs {
			sigHex, err := roundwire.EncodePartialSignature(sig)
			if err != nil {
				log.ErrorS(nil, "Encode partial signature failed", err)

				return nil
			}

			entry.Signatures = append(entry.Signatures,
				&roundwire.TxSigEntry{
					TxIdHex:      txID.String(),
					SignatureHex: sigHex,
				},
			)
		}

		roundwire.SortTxSigEntries(entry.Signatures)
		payload.Entries = append(payload.Entries, entry)
	}

	roundwire.SortSignerSigBundles(payload.Entries)

	msg, err := roundwire.WrapPayload(payload)
	if err != nil {
		log.ErrorS(nil, "Wrap SubmitPartialSigRequest payload failed",
			err)

		return nil
	}

	return msg
}

// RPCService returns the mailbox RPC service for this message.
func (m *SubmitForfeitSigRequest) RPCService() string {
	return roundwire.ServiceName
}

// RPCMethod returns the mailbox RPC method for this message.
func (m *SubmitForfeitSigRequest) RPCMethod() string {
	return roundwire.MethodSubmitForfeitSigRequest
}

// ToProto converts SubmitForfeitSigRequest to a protobuf payload message.
func (m *SubmitForfeitSigRequest) ToProto() proto.Message {
	payload := &roundwire.SubmitForfeitSigsPayload{
		RoundId: m.RoundID.String(),
		Signatures: make(
			[]*roundwire.BoardingInputSigPayload, 0,
			len(m.Signatures),
		),
	}

	for _, sig := range m.Signatures {
		encodedOutpoint := roundwire.EncodeOutPoint(sig.Outpoint)
		payload.Signatures = append(payload.Signatures,
			&roundwire.BoardingInputSigPayload{
				InputIndex: int32(sig.InputIndex),
				Outpoint: &roundwire.OutPointPayload{
					TxId: encodedOutpoint.TxId,
					Vout: encodedOutpoint.Vout,
				},
				SignatureHex: roundwire.EncodeSchnorrSignature(
					sig.ClientSignature,
				),
			},
		)
	}

	sort.Slice(payload.Signatures, func(i, j int) bool {
		if payload.Signatures[i].Outpoint.TxId !=
			payload.Signatures[j].Outpoint.TxId {

			return payload.Signatures[i].Outpoint.TxId <
				payload.Signatures[j].Outpoint.TxId
		}

		if payload.Signatures[i].Outpoint.Vout !=
			payload.Signatures[j].Outpoint.Vout {

			return payload.Signatures[i].Outpoint.Vout <
				payload.Signatures[j].Outpoint.Vout
		}

		return payload.Signatures[i].InputIndex <
			payload.Signatures[j].InputIndex
	})

	msg, err := roundwire.WrapPayload(payload)
	if err != nil {
		log.ErrorS(nil, "Wrap SubmitForfeitSigRequest payload failed",
			err)

		return nil
	}

	return msg
}

// ForfeitRequestToVTXO is emitted by the FSM when a VTXO must sign a forfeit
// transaction as part of a batch swap. The round actor routes this message to
// the VTXO actor via its service key. The VTXO actor should sign the forfeit
// transaction and respond with ForfeitSignatureResponse.
//
// This message contains all information needed to construct and sign the
// forfeit transaction:
//   - Connector output from new commitment tx (links forfeit atomically)
//   - Server's forfeit address (where forfeited value is paid)
type ForfeitRequestToVTXO struct {
	actor.BaseMessage

	// VTXOOutpoint identifies the VTXO being forfeited.
	VTXOOutpoint wire.OutPoint

	// RoundID is the new round where the refreshed VTXO will be created.
	RoundID string

	// ConnectorOutpoint is the connector output from the new commitment tx
	// that the forfeit tx must spend. This links the forfeit atomically to
	// the new round - the forfeit is only valid if the new round confirms.
	ConnectorOutpoint wire.OutPoint

	// ConnectorPkScript is the scriptPubKey of the connector output.
	ConnectorPkScript []byte

	// ConnectorAmount is the value of the connector output in satoshis.
	ConnectorAmount int64

	// ServerForfeitPkScript is the operator's taproot script where the
	// forfeited VTXO value will be paid.
	ServerForfeitPkScript []byte
}

func (m *ForfeitRequestToVTXO) clientOutMsgSealed() {}

// MessageType returns the message type for logging.
func (m *ForfeitRequestToVTXO) MessageType() string {
	return "ForfeitRequestToVTXO"
}

// ForfeitConfirmedToVTXO is emitted by the FSM when the commitment transaction
// confirms, indicating that the forfeit is final. The round actor routes this
// to old VTXO actors so they can transition to the terminal Forfeited state.
type ForfeitConfirmedToVTXO struct {
	actor.BaseMessage

	// VTXOOutpoint identifies the forfeited VTXO.
	VTXOOutpoint wire.OutPoint

	// CommitmentTxID is the new commitment transaction that confirmed.
	CommitmentTxID chainhash.Hash

	// BlockHeight is the height at which confirmation occurred.
	BlockHeight int32
}

func (m *ForfeitConfirmedToVTXO) clientOutMsgSealed() {}

// MessageType returns the message type for logging.
func (m *ForfeitConfirmedToVTXO) MessageType() string {
	return "ForfeitConfirmedToVTXO"
}

// SubmitVTXOForfeitSigsToServer is emitted by the FSM after collecting all
// forfeit signatures from VTXO actors. This message contains the signatures
// for all VTXOs being refreshed in the round and is sent to the server so it
// can complete the forfeit transactions.
type SubmitVTXOForfeitSigsToServer struct {
	actor.BaseMessage

	// RoundID identifies the round.
	RoundID RoundID

	// ForfeitSigs maps VTXO outpoints to their forfeit transaction
	// signatures. Each signature is the client's schnorr signature for the
	// collaborative 2-of-2 spend from the VTXO.
	ForfeitSigs map[wire.OutPoint]*schnorr.Signature

	// ForfeitTxs maps VTXO outpoints to the built forfeit transactions.
	// The server uses these to broadcast after adding its signature.
	ForfeitTxs map[wire.OutPoint]*wire.MsgTx
}

func (m *SubmitVTXOForfeitSigsToServer) clientOutMsgSealed() {}

// MessageType returns the message type for logging.
func (m *SubmitVTXOForfeitSigsToServer) MessageType() string {
	return "SubmitVTXOForfeitSigsToServer"
}

// RPCService returns the mailbox RPC service for this message.
func (m *SubmitVTXOForfeitSigsToServer) RPCService() string {
	return roundwire.ServiceName
}

// RPCMethod returns the mailbox RPC method for this message.
func (m *SubmitVTXOForfeitSigsToServer) RPCMethod() string {
	return roundwire.MethodSubmitVTXOForfeitSigsRequest
}

// ToProto converts SubmitVTXOForfeitSigsToServer to a protobuf payload.
func (m *SubmitVTXOForfeitSigsToServer) ToProto() proto.Message {
	payload := &roundwire.SubmitVTXOForfeitSigsPayload{
		RoundId: m.RoundID.String(),
		Entries: make(
			[]*roundwire.VTXOForfeitSigPayload, 0,
			len(m.ForfeitSigs),
		),
	}

	for outpoint, sig := range m.ForfeitSigs {
		unsignedTx, ok := m.ForfeitTxs[outpoint]
		if !ok {
			log.ErrorS(nil,
				"Missing forfeit tx for outpoint in "+
					"SubmitVTXOForfeitSigsToServer",
				fmt.Errorf("forfeit tx missing"),
			)

			return nil
		}

		txHex, err := roundwire.EncodeMsgTx(unsignedTx)
		if err != nil {
			log.ErrorS(nil, "Encode forfeit tx failed", err)

			return nil
		}

		encodedOutpoint := roundwire.EncodeOutPoint(outpoint)
		payload.Entries = append(payload.Entries,
			&roundwire.VTXOForfeitSigPayload{
				VtxoOutpoint: &roundwire.OutPointPayload{
					TxId: encodedOutpoint.TxId,
					Vout: encodedOutpoint.Vout,
				},
				SignatureHex: roundwire.EncodeSchnorrSignature(
					sig,
				),
				UnsignedTxHex: txHex,
			},
		)
	}

	sort.Slice(payload.Entries, func(i, j int) bool {
		if payload.Entries[i].VtxoOutpoint.TxId !=
			payload.Entries[j].VtxoOutpoint.TxId {

			return payload.Entries[i].VtxoOutpoint.TxId <
				payload.Entries[j].VtxoOutpoint.TxId
		}

		return payload.Entries[i].VtxoOutpoint.Vout <
			payload.Entries[j].VtxoOutpoint.Vout
	})

	msg, err := roundwire.WrapPayload(payload)
	if err != nil {
		log.ErrorS(nil,
			"Wrap SubmitVTXOForfeitSigsToServer payload failed",
			err,
		)

		return nil
	}

	return msg
}

func joinRoundRequestPayload(
	msg *JoinRoundRequest,
) (*roundwire.JoinRoundRequestPayload, error) {

	payload := &roundwire.JoinRoundRequestPayload{
		RoundId:    msg.RoundID,
		Identifier: roundwire.EncodePubKey(msg.Identifier),
		BoardingRequests: make(
			[]*roundwire.BoardingRequestPayload, 0,
			len(msg.BoardingRequests),
		),
		VtxoRequests: make(
			[]*roundwire.VTXORequestPayload, 0,
			len(msg.VTXORequests),
		),
		ForfeitRequests: make(
			[]*roundwire.ForfeitRequestPayload, 0,
			len(msg.ForfeitRequests),
		),
		LeaveRequests: make(
			[]*roundwire.LeaveRequestPayload, 0,
			len(msg.LeaveRequests),
		),
	}

	for i := range msg.BoardingRequests {
		req := msg.BoardingRequests[i]
		if req.Outpoint == nil {
			return nil, fmt.Errorf(
				"boarding request %d has nil outpoint", i,
			)
		}

		if req.TxProof.IsSome() {
			return nil, fmt.Errorf(
				"boarding request %d has unsupported tx proof",
				i,
			)
		}

		outpoint := roundwire.EncodeOutPoint(*req.Outpoint)
		payload.BoardingRequests = append(
			payload.BoardingRequests,
			&roundwire.BoardingRequestPayload{
				Outpoint: &roundwire.OutPointPayload{
					TxId: outpoint.TxId,
					Vout: outpoint.Vout,
				},
				ClientKey: roundwire.EncodePubKey(
					req.ClientKey,
				),
				OperatorKey: roundwire.EncodePubKey(
					req.OperatorKey,
				),
				ExitDelay: req.ExitDelay,
			},
		)
	}

	for i := range msg.VTXORequests {
		req := msg.VTXORequests[i]
		signingKey := roundwire.EncodeKeyDescriptor(req.SigningKey)
		payload.VtxoRequests = append(
			payload.VtxoRequests,
			&roundwire.VTXORequestPayload{
				Amount: int64(req.Amount),
				PkScriptHex: hex.EncodeToString(
					req.PkScript,
				),
				Expiry: req.Expiry,
				ClientKey: roundwire.EncodePubKey(
					req.ClientKey,
				),
				OperatorKey: roundwire.EncodePubKey(
					req.OperatorKey,
				),
				SigningKey: &roundwire.KeyDescriptorPayload{
					KeyFamily: signingKey.KeyFamily,
					KeyIndex:  signingKey.KeyIndex,
					PubKeyHex: signingKey.PubKeyHex,
				},
			},
		)
	}

	for _, req := range msg.ForfeitRequests {
		encodedOutpoint := roundwire.EncodeOutPoint(req.VTXOOutpoint)
		payload.ForfeitRequests = append(
			payload.ForfeitRequests,
			&roundwire.ForfeitRequestPayload{
				VtxoOutpoint: &roundwire.OutPointPayload{
					TxId: encodedOutpoint.TxId,
					Vout: encodedOutpoint.Vout,
				},
			},
		)
	}

	for i := range msg.LeaveRequests {
		req := msg.LeaveRequests[i]
		output := roundwire.EncodeTxOut(req.Output)
		payload.LeaveRequests = append(
			payload.LeaveRequests,
			&roundwire.LeaveRequestPayload{
				Output: &roundwire.TxOutPayload{
					Value:    output.Value,
					PkScript: output.PkScript,
				},
			},
		)
	}

	if msg.Auth != nil {
		payload.Auth = &roundwire.JoinRoundAuthPayload{
			MessageHex:   hex.EncodeToString(msg.Auth.Message),
			ValidFrom:    msg.Auth.ValidFrom,
			ValidUntil:   msg.Auth.ValidUntil,
			SignatureHex: hex.EncodeToString(msg.Auth.Signature),
		}
	}

	return payload, nil
}

// RegisterConfirmationRequest is emitted by the FSM to request chain monitoring
// for a transaction. The actor will complete this message with the NotifyActor
// field before sending to ChainSource.
//
// This implements ClientEvent so it can be emitted via Outbox. The actor will
// convert this to a chainsource.RegisterConfRequest and add the NotifyActor
// field pointing to itself.
type RegisterConfirmationRequest struct {
	actor.BaseMessage

	// CallerID is a unique identifier for this monitoring request. This is
	// used by ChainSource to construct the service key for the dedicated
	// confirmation actor.
	CallerID string

	// PkScript is the public key script to monitor.
	PkScript []byte

	// Txid is optional and, if set, instructs the monitoring backend to
	// watch for confirmations of the specific transaction.
	Txid *chainhash.Hash

	// TargetConfs is the number of confirmations to wait for.
	TargetConfs uint32

	// HeightHint is an optional height hint indicating the earliest block
	// that could contain the transaction. Set to 0 if unknown.
	HeightHint uint32
}

func (m *RegisterConfirmationRequest) clientOutMsgSealed() {}

// VTXOCreatedNotification notifies higher layers (wallet, VTXO manager) that
// new VTXOs are available after successful boarding. This is emitted once the
// commitment transaction confirms and includes the full descriptors (with tree
// paths) so the wallet can resume or unroll on-chain if needed.
//
// Note: TreeDepth is per-VTXO and derivable from ClientVTXO.TreePath.
type VTXOCreatedNotification struct {
	actor.BaseMessage

	// VTXOs are the ClientVTXOs created by this round.
	VTXOs []*ClientVTXO

	// RoundID identifies the round that created these VTXOs.
	RoundID string

	// CommitmentTxID is the txid of the confirmed commitment transaction.
	CommitmentTxID chainhash.Hash

	// BatchExpiry is the absolute block height when the batch expires.
	BatchExpiry int32

	// CreatedHeight is the block height when the commitment tx confirmed.
	CreatedHeight int32
}

// MessageType returns the message type identifier for logging and debugging.
func (m *VTXOCreatedNotification) MessageType() string {
	return "VTXOCreatedNotification"
}

func (m *VTXOCreatedNotification) clientOutMsgSealed() {}

// VTXOManagerMsg implements actormsg.VTXOManagerMsg marker interface.
func (m *VTXOCreatedNotification) VTXOManagerMsg() {}

// RoundCompletedNotification is emitted when a round FSM reaches ConfirmedState
// which signals the actor to perform cleanup (remove from activeRounds,
// finalize storage). This replaces the need for manual state inspection via
// checkRoundCompletion().
type RoundCompletedNotification struct {
	actor.BaseMessage

	// RoundID identifies the completed round.
	RoundID RoundID

	// TxID is the confirmed commitment transaction ID.
	TxID chainhash.Hash

	// ConfInfo contains the block height and hash at which the commitment
	// tx was confirmed.
	ConfInfo ConfInfo
}

func (m *RoundCompletedNotification) clientOutMsgSealed() {}

// RoundCheckpointedNotification is emitted by the primary FSM when it reaches
// InputSigSentState. This signals that a round has been checkpointed to
// storage and should be migrated to a dedicated round FSM. This replaces the
// need for manual state inspection via checkPrimaryFSMForNewRound().
type RoundCheckpointedNotification struct {
	actor.BaseMessage

	// RoundID identifies the checkpointed round to migrate.
	RoundID RoundID
}

func (m *RoundCheckpointedNotification) clientOutMsgSealed() {}

// RoundFailedNotification is emitted when a round FSM transitions to
// ClientFailedState. This notifies higher layers (actor, wallet) of the
// failure so they can update UI, trigger recovery flows, or clean up
// resources. The server may also be notified to abort the round.
type RoundFailedNotification struct {
	actor.BaseMessage

	// RoundID identifies the failed round. None if the failure occurred
	// before a round was assigned.
	RoundID fn.Option[RoundID]

	// Reason is a human-readable description of the failure.
	Reason string

	// Recoverable indicates if the client can retry the round or if CSV
	// recovery is needed.
	Recoverable bool

	// OriginalError contains the underlying error for logging/debugging.
	OriginalError error
}

func (m *RoundFailedNotification) clientOutMsgSealed() {}
