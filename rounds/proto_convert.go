// Proto-to-domain conversion helpers for inbound round RPC requests.
//
// These functions translate protobuf request types from the roundpb
// package into domain types consumed by the rounds actor. They are
// called from the server wiring layer's AddEnvelopeRoute Adapt
// closures (server_rounds.go) and were previously private methods
// inside the RoundOperator handler.

package rounds

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/taproot-assets/proof"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/routing/route"
)

// JoinRoundRequestFromProto converts a roundpb.JoinRoundRequest to
// the domain types.JoinRoundRequest. Each sub-request type (boarding,
// VTXO, forfeit, leave) is converted using the roundpb helper
// functions for outpoints, public keys, and transaction outputs.
func JoinRoundRequestFromProto(
	req *roundpb.JoinRoundRequest) (*types.JoinRoundRequest, error) {

	// Parse the participant identifier (33-byte compressed
	// public key).
	identifier, err := btcec.ParsePubKey(req.GetIdentifier())
	if err != nil {
		return nil, fmt.Errorf(
			"parse identifier pubkey: %w", err,
		)
	}

	// Convert boarding requests.
	boardingReqs := make(
		[]*types.BoardingRequest,
		0, len(req.GetBoardingRequests()),
	)
	for i, br := range req.GetBoardingRequests() {
		domainBR, err := boardingRequestFromProto(br)
		if err != nil {
			return nil, fmt.Errorf(
				"boarding_request[%d]: %w", i, err,
			)
		}

		boardingReqs = append(boardingReqs, domainBR)
	}

	// Convert VTXO requests.
	vtxoReqs := make(
		[]*types.VTXORequest,
		0, len(req.GetVtxoRequests()),
	)
	for i, vr := range req.GetVtxoRequests() {
		domainVR, err := vtxoRequestFromProto(vr)
		if err != nil {
			return nil, fmt.Errorf(
				"vtxo_request[%d]: %w", i, err,
			)
		}

		vtxoReqs = append(vtxoReqs, domainVR)
	}

	// Convert forfeit requests.
	forfeitReqs := make(
		[]*types.ForfeitRequest,
		0, len(req.GetForfeitRequests()),
	)
	for i, fr := range req.GetForfeitRequests() {
		op, err := roundpb.OutpointFromProto(
			fr.GetVtxoOutpoint(),
		)
		if err != nil {
			return nil, fmt.Errorf(
				"forfeit_request[%d]: %w", i, err,
			)
		}

		forfeitReqs = append(forfeitReqs, &types.ForfeitRequest{
			VTXOOutpoint: &op,
		})
	}

	// Convert leave requests.
	leaveReqs := make(
		[]*types.LeaveRequest,
		0, len(req.GetLeaveRequests()),
	)
	for i, lr := range req.GetLeaveRequests() {
		txOut, err := roundpb.TxOutFromProto(lr.GetOutput())
		if err != nil {
			return nil, fmt.Errorf(
				"leave_request[%d]: %w", i, err,
			)
		}

		leaveReqs = append(leaveReqs, &types.LeaveRequest{
			Output: txOut,
		})
	}

	// Convert auth payload if present.
	var auth *types.JoinRoundAuth
	if req.GetAuth() != nil {
		auth = &types.JoinRoundAuth{
			Message:    req.Auth.GetMessage(),
			ValidFrom:  req.Auth.GetValidFrom(),
			ValidUntil: req.Auth.GetValidUntil(),
			Signature:  req.Auth.GetSignature(),
		}
	}

	return &types.JoinRoundRequest{
		Identifier:   identifier,
		VTXOReqs:     vtxoReqs,
		BoardingReqs: boardingReqs,
		LeaveReqs:    leaveReqs,
		ForfeitReqs:  forfeitReqs,
		Auth:         auth,
	}, nil
}

// boardingRequestFromProto converts a single proto BoardingRequest
// to the domain types.BoardingRequest. If the proto includes a
// non-empty tx_proof, it is deserialized into the domain TxProof
// field for server-side SPV validation.
func boardingRequestFromProto(
	br *roundpb.BoardingRequest) (*types.BoardingRequest, error) {

	op, err := roundpb.OutpointFromProto(br.GetOutpoint())
	if err != nil {
		return nil, fmt.Errorf("outpoint: %w", err)
	}

	// Deserialize the TxProof if provided by the client.
	var txProof fn.Option[proof.TxProof]
	if len(br.GetTxProof()) > 0 {
		p, err := types.DeserializeTxProof(br.GetTxProof())
		if err != nil {
			return nil, fmt.Errorf("tx_proof: %w", err)
		}
		if p != nil {
			txProof = fn.Some(*p)
		}
	}

	req := &types.BoardingRequest{
		Outpoint:       &op,
		PolicyTemplate: bytes.Clone(br.GetPolicyTemplate()),
		TxProof:        txProof,
	}

	template, err := req.DecodePolicyTemplate()
	if err != nil {
		return nil, fmt.Errorf("policy_template: %w", err)
	}

	params, err := arkscript.DecodeStandardVTXOParams(template)
	if err != nil {
		return nil, fmt.Errorf("policy_template: %w", err)
	}

	req.ClientKey = params.OwnerKey
	req.OperatorKey = params.OperatorKey
	req.ExitDelay = params.ExitDelay

	return req, nil
}

// vtxoRequestFromProto converts a single proto VTXORequest to the
// domain types.VTXORequest. The SigningKey PubKey is populated from
// the proto signing_key field; the KeyLocator is left at zero since
// it is a client-side concern.
func vtxoRequestFromProto(
	vr *roundpb.VTXORequest) (*types.VTXORequest, error) {

	signingPub, err := btcec.ParsePubKey(vr.GetSigningKey())
	if err != nil {
		return nil, fmt.Errorf("signing_key: %w", err)
	}

	req := &types.VTXORequest{
		Amount:         btcutil.Amount(vr.GetAmount()),
		PolicyTemplate: bytes.Clone(vr.GetPolicyTemplate()),
		SigningKey: keychain.KeyDescriptor{
			PubKey: signingPub,
		},
	}

	template, err := req.DecodePolicyTemplate()
	if err != nil {
		return nil, fmt.Errorf("policy_template: %w", err)
	}

	req.PkScript, err = template.PkScript()
	if err != nil {
		return nil, fmt.Errorf("pk_script: %w", err)
	}

	params, err := arkscript.DecodeStandardVTXOParams(template)
	if err == nil {
		req.ClientKey = params.OwnerKey
		req.OperatorKey = params.OperatorKey
		req.Expiry = params.ExitDelay
	}

	return req, nil
}

// ParseRoundID parses a 16-byte UUID from the proto round_id field
// into a RoundID.
func ParseRoundID(raw []byte) (RoundID, error) {
	if len(raw) != 16 {
		return RoundID{}, fmt.Errorf(
			"invalid round_id length: %d, want 16",
			len(raw),
		)
	}

	var id RoundID
	copy(id[:], raw)

	return id, nil
}

// NoncesFromProto converts the proto nonce map into the domain
// representation used by ClientVTXONoncesEvent. The outer map key
// is a signing key hex string (33-byte compressed pubkey), and the
// inner map key is a transaction ID.
func NoncesFromProto(
	protoNonces map[string]*roundpb.SignerNonces) (
	map[SigningKeyHex]map[tree.TxID]tree.Musig2PubNonce, error) {

	result := make(
		map[SigningKeyHex]map[tree.TxID]tree.Musig2PubNonce,
		len(protoNonces),
	)

	for keyHex, sn := range protoNonces {
		signingKey, err := route.NewVertexFromStr(keyHex)
		if err != nil {
			return nil, fmt.Errorf(
				"signing key %q: %w", keyHex, err,
			)
		}

		txNonces := make(
			map[tree.TxID]tree.Musig2PubNonce,
			len(sn.GetTxNonces()),
		)
		for txIDHex, nonceBytes := range sn.GetTxNonces() {
			txID, err := roundpb.TxIDFromHex(txIDHex)
			if err != nil {
				return nil, fmt.Errorf(
					"tx_id %q: %w",
					txIDHex, err,
				)
			}

			if len(nonceBytes) != musig2.PubNonceSize {
				return nil, fmt.Errorf(
					"nonce for tx %s: want %d "+
						"bytes, got %d",
					txIDHex,
					musig2.PubNonceSize,
					len(nonceBytes),
				)
			}

			var nonce tree.Musig2PubNonce
			copy(nonce[:], nonceBytes)

			txNonces[txID] = nonce
		}

		result[signingKey] = txNonces
	}

	return result, nil
}

// PartialSigsFromProto converts the proto partial signature map into
// the domain representation used by ClientVTXOPartialSigsEvent.
func PartialSigsFromProto(
	protoSigs map[string]*roundpb.SignerPartialSigs) (
	map[SigningKeyHex]map[tree.TxID]*musig2.PartialSignature,
	error) {

	result := make(
		map[SigningKeyHex]map[tree.TxID]*musig2.PartialSignature,
		len(protoSigs),
	)

	for keyHex, sp := range protoSigs {
		signingKey, err := route.NewVertexFromStr(keyHex)
		if err != nil {
			return nil, fmt.Errorf(
				"signing key %q: %w", keyHex, err,
			)
		}

		txSigs := make(
			map[tree.TxID]*musig2.PartialSignature,
			len(sp.GetTxSigs()),
		)
		for txIDHex, sigBytes := range sp.GetTxSigs() {
			txID, err := roundpb.TxIDFromHex(txIDHex)
			if err != nil {
				return nil, fmt.Errorf(
					"tx_id %q: %w",
					txIDHex, err,
				)
			}

			var pSig musig2.PartialSignature
			err = pSig.Decode(
				bytes.NewReader(sigBytes),
			)
			if err != nil {
				return nil, fmt.Errorf(
					"partial sig for tx %s: %w",
					txIDHex, err,
				)
			}

			txSigs[txID] = &pSig
		}

		result[signingKey] = txSigs
	}

	return result, nil
}

// BoardingInputSigsFromProto converts proto BoardingInputSignature
// entries to the domain types.BoardingInputSignature slice.
func BoardingInputSigsFromProto(
	pbSigs []*roundpb.BoardingInputSignature) (
	[]*types.BoardingInputSignature, error) {

	sigs := make(
		[]*types.BoardingInputSignature, 0, len(pbSigs),
	)

	for i, pb := range pbSigs {
		op, err := roundpb.OutpointFromProto(
			pb.GetOutpoint(),
		)
		if err != nil {
			return nil, fmt.Errorf(
				"signature[%d] outpoint: %w", i, err,
			)
		}

		clientSig, err := roundpb.SchnorrSigFromBytes(
			pb.GetClientSignature(),
		)
		if err != nil {
			return nil, fmt.Errorf(
				"signature[%d] client_sig: %w", i, err,
			)
		}

		sigs = append(sigs, &types.BoardingInputSignature{
			InputIndex:      int(pb.GetInputIndex()),
			Outpoint:        op,
			ClientSignature: clientSig,
		})
	}

	return sigs, nil
}

// ForfeitTxSigsFromProto converts proto ForfeitTxSig entries to
// the domain types.ForfeitTxSig slice.
func ForfeitTxSigsFromProto(
	pbSigs []*roundpb.ForfeitTxSig) (
	[]*types.ForfeitTxSig, error) {

	sigs := make([]*types.ForfeitTxSig, 0, len(pbSigs))

	for i, pb := range pbSigs {
		unsignedTx, err := roundpb.MsgTxFromBytes(
			pb.GetUnsignedTx(),
		)
		if err != nil {
			return nil, fmt.Errorf(
				"forfeit_tx[%d] unsigned_tx: %w",
				i, err,
			)
		}

		vtxoSig, err := roundpb.SchnorrSigFromBytes(
			pb.GetClientVtxoSig(),
		)
		if err != nil {
			return nil, fmt.Errorf(
				"forfeit_tx[%d] client_vtxo_sig: %w",
				i, err,
			)
		}

		spendPath, err := arkscript.DecodeSpendPath(
			pb.GetSpendPath(),
		)
		if err != nil {
			return nil, fmt.Errorf(
				"forfeit_tx[%d] spend_path: %w", i, err,
			)
		}

		sigs = append(sigs, &types.ForfeitTxSig{
			UnsignedTx:    unsignedTx,
			ClientVTXOSig: vtxoSig,
			SpendPath:     spendPath,
		})
	}

	return sigs, nil
}

// Compile-time check that all client-facing outbox events satisfy
// ClientMessage for bridge delivery.
var _ clientconn.ClientMessage = (*ClientErrorResp)(nil)
var _ clientconn.ClientMessage = (*ClientSuccessResp)(nil)
var _ clientconn.ClientMessage = (*ClientAwaitingInputSigsResp)(nil)
var _ clientconn.ClientMessage = (*ClientVTXOAggNonces)(nil)
var _ clientconn.ClientMessage = (*ClientVTXOAggSigs)(nil)
var _ clientconn.ClientMessage = (*ClientBatchInfo)(nil)
var _ clientconn.ClientMessage = (*ClientRoundFailedResp)(nil)

// Ensure unused imports compile. The wire package is used in
// ForfeitTxSigsFromProto via roundpb.MsgTxFromBytes which returns
// *wire.MsgTx.
var _ = (*wire.MsgTx)(nil)
