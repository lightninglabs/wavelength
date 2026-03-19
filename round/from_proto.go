package round

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	"github.com/lightningnetwork/lnd/keychain"
	"google.golang.org/protobuf/proto"
)

// FromProto populates a RoundJoined from a ClientSuccessResp proto. The round
// ID is parsed as a 16-byte UUID and outpoints are converted from their proto
// representation.
func (e *RoundJoined) FromProto(p proto.Message) error {
	pb, ok := p.(*roundpb.ClientSuccessResp)
	if !ok {
		return fmt.Errorf(
			"unexpected proto type: %T, want "+
				"*roundpb.ClientSuccessResp", p,
		)
	}

	// Parse round ID from 16-byte UUID.
	if len(pb.RoundId) != 16 {
		return fmt.Errorf(
			"invalid round_id length: %d", len(pb.RoundId),
		)
	}
	copy(e.RoundID[:], pb.RoundId)

	// Convert accepted boarding outpoints.
	boardingOps, err := roundpb.OutpointsFromProto(
		pb.AcceptedBoardingOutpoints,
	)
	if err != nil {
		return fmt.Errorf(
			"accepted_boarding_outpoints: %w", err,
		)
	}
	e.AcceptedBoardingOutpoints = boardingOps

	// Convert accepted VTXO outpoints.
	vtxoOps, err := roundpb.OutpointsFromProto(
		pb.AcceptedVtxoOutpoints,
	)
	if err != nil {
		return fmt.Errorf(
			"accepted_vtxo_outpoints: %w", err,
		)
	}
	e.AcceptedVTXOOutpoints = vtxoOps

	return nil
}

// FromProto populates a CommitmentTxBuilt from a ClientBatchInfo proto. The
// PSBT is deserialized from bytes, VTXO trees are reconstructed from their
// flattened representation, and the connector leaf map is converted from
// string-keyed proto entries.
func (e *CommitmentTxBuilt) FromProto(p proto.Message) error {
	pb, ok := p.(*roundpb.ClientBatchInfo)
	if !ok {
		return fmt.Errorf(
			"unexpected proto type: %T, want "+
				"*roundpb.ClientBatchInfo", p,
		)
	}

	// Parse round ID.
	if len(pb.RoundId) != 16 {
		return fmt.Errorf(
			"invalid round_id length: %d", len(pb.RoundId),
		)
	}
	copy(e.RoundID[:], pb.RoundId)

	// Deserialize PSBT. The batch PSBT is semantically required: it
	// carries the unsigned commitment transaction with WitnessUtxo
	// data needed for Taproot sighash computation.
	tx, err := roundpb.PSBTFromBytes(pb.BatchPsbt)
	if err != nil {
		return fmt.Errorf("batch_psbt: %w", err)
	}
	if tx == nil {
		return fmt.Errorf("batch_psbt: required field is empty")
	}
	e.Tx = tx

	// Convert VTXO tree paths. Reject negative indices since they
	// are semantically invalid as commitment tx output indices.
	if pb.VtxoTreePaths != nil {
		e.VTXOTreePaths = make(
			map[int]*tree.Tree, len(pb.VtxoTreePaths),
		)
		for idx, pt := range pb.VtxoTreePaths {
			if idx < 0 {
				return fmt.Errorf(
					"vtxo_tree_paths: negative "+
						"index %d", idx,
				)
			}

			t, treeErr := roundpb.TreeFromProto(
				pt, e.TreeOpts...,
			)
			if treeErr != nil {
				return fmt.Errorf(
					"vtxo_tree_paths[%d]: %w",
					idx, treeErr,
				)
			}
			e.VTXOTreePaths[int(idx)] = t
		}
	}

	// Convert connector leaf map. The server sends ConnectorLeafInfo
	// with LeafOutpoint and LeafOutput, which maps to the client's
	// ConnectorLeafInfo with ConnectorOutpoint, ConnectorPkScript, and
	// ConnectorAmount. VTXOAmount is looked up from local state later.
	if pb.ConnectorLeafMap != nil {
		e.ForfeitMappings = make(
			map[wire.OutPoint]*ConnectorLeafInfo,
			len(pb.ConnectorLeafMap),
		)
		for key, info := range pb.ConnectorLeafMap {
			op, opErr := roundpb.OutpointFromMapKey(key)
			if opErr != nil {
				return fmt.Errorf(
					"connector_leaf_map key %q: %w",
					key, opErr,
				)
			}

			connOP, connErr := roundpb.OutpointFromProto(
				info.LeafOutpoint,
			)
			if connErr != nil {
				return fmt.Errorf(
					"connector_leaf_map[%s] "+
						"leaf_outpoint: %w",
					key, connErr,
				)
			}

			leafOut, leafErr := roundpb.TxOutFromProto(
				info.LeafOutput,
			)
			if leafErr != nil {
				return fmt.Errorf(
					"connector_leaf_map[%s] "+
						"leaf_output: %w",
					key, leafErr,
				)
			}
			if leafOut == nil {
				return fmt.Errorf(
					"connector_leaf_map[%s] "+
						"nil leaf_output", key,
				)
			}

			e.ForfeitMappings[op] = &ConnectorLeafInfo{
				ConnectorOutpoint: connOP,
				ConnectorPkScript: leafOut.PkScript,
				ConnectorAmount:   leafOut.Value,
			}
		}
	}

	return nil
}

// FromProto populates an AwaitingBoardingSigs from a
// ClientAwaitingInputSigsResp proto.
func (e *AwaitingBoardingSigs) FromProto(p proto.Message) error {
	pb, ok := p.(*roundpb.ClientAwaitingInputSigsResp)
	if !ok {
		return fmt.Errorf(
			"unexpected proto type: %T, want "+
				"*roundpb.ClientAwaitingInputSigsResp", p,
		)
	}

	if len(pb.RoundId) != 16 {
		return fmt.Errorf(
			"invalid round_id length: %d", len(pb.RoundId),
		)
	}
	copy(e.RoundID[:], pb.RoundId)

	return nil
}

// FromProto populates a NoncesAggregated from a ClientVTXOAggNonces proto.
// Transaction IDs are decoded from hex and nonces are copied from their byte
// representation into fixed-size arrays.
func (e *NoncesAggregated) FromProto(p proto.Message) error {
	pb, ok := p.(*roundpb.ClientVTXOAggNonces)
	if !ok {
		return fmt.Errorf(
			"unexpected proto type: %T, want "+
				"*roundpb.ClientVTXOAggNonces", p,
		)
	}

	if len(pb.RoundId) != 16 {
		return fmt.Errorf(
			"invalid round_id length: %d", len(pb.RoundId),
		)
	}
	copy(e.RoundID[:], pb.RoundId)

	// Convert aggregated nonces map. Keys are hex-encoded TxIDs,
	// values are 66-byte MuSig2 public nonces.
	if pb.AggNonces != nil {
		e.AggNonces = make(
			map[tree.TxID]tree.Musig2PubNonce,
			len(pb.AggNonces),
		)
		for hexID, nonceBytes := range pb.AggNonces {
			txID, err := roundpb.TxIDFromHex(hexID)
			if err != nil {
				return fmt.Errorf(
					"agg_nonces key: %w", err,
				)
			}

			var expected tree.Musig2PubNonce
			if len(nonceBytes) != len(expected) {
				return fmt.Errorf(
					"agg_nonces[%s] invalid nonce "+
						"length: %d",
					hexID, len(nonceBytes),
				)
			}

			var nonce tree.Musig2PubNonce
			copy(nonce[:], nonceBytes)
			e.AggNonces[txID] = nonce
		}
	}

	return nil
}

// FromProto populates an OperatorSigned from a ClientVTXOAggSigs proto.
// Transaction IDs are decoded from hex and signatures are parsed from their
// serialized byte representation.
func (e *OperatorSigned) FromProto(p proto.Message) error {
	pb, ok := p.(*roundpb.ClientVTXOAggSigs)
	if !ok {
		return fmt.Errorf(
			"unexpected proto type: %T, want "+
				"*roundpb.ClientVTXOAggSigs", p,
		)
	}

	if len(pb.RoundId) != 16 {
		return fmt.Errorf(
			"invalid round_id length: %d", len(pb.RoundId),
		)
	}
	copy(e.RoundID[:], pb.RoundId)

	// Convert aggregated signatures map. Keys are hex-encoded TxIDs,
	// values are 64-byte schnorr signatures.
	if pb.AggSigs != nil {
		e.AggSigs = make(
			map[tree.TxID]*schnorr.Signature,
			len(pb.AggSigs),
		)
		for hexID, sigBytes := range pb.AggSigs {
			txID, err := roundpb.TxIDFromHex(hexID)
			if err != nil {
				return fmt.Errorf(
					"agg_sigs key: %w", err,
				)
			}

			sig, sigErr := roundpb.SchnorrSigFromBytes(
				sigBytes,
			)
			if sigErr != nil {
				return fmt.Errorf(
					"agg_sigs[%s]: %w",
					hexID, sigErr,
				)
			}

			e.AggSigs[txID] = sig
		}
	}

	return nil
}

// FromProto populates a BoardingFailed from a ClientRoundFailedResp proto.
// All server-initiated round failures are treated as recoverable by default.
func (e *BoardingFailed) FromProto(p proto.Message) error {
	switch pb := p.(type) {
	case *roundpb.ClientRoundFailedResp:
		e.Reason = pb.Reason
		e.Recoverable = true

		return nil

	case *roundpb.ClientErrorResp:
		// ClientErrorResp is also mapped to BoardingFailed, matching
		// the bridge's convertToClientEvent behavior.
		e.Reason = pb.ErrorMsg
		e.Recoverable = true

		return nil

	default:
		return fmt.Errorf(
			"unexpected proto type: %T, want "+
				"*roundpb.ClientRoundFailedResp or "+
				"*roundpb.ClientErrorResp", p,
		)
	}
}

// FromProto populates a JoinRoundRequest from its proto representation. This
// is the inverse of ToProto and is used by test code that needs to
// deserialize captured mailbox envelopes back into domain types.
func (m *JoinRoundRequest) FromProto(p proto.Message) error {
	pb, ok := p.(*roundpb.JoinRoundRequest)
	if !ok {
		return fmt.Errorf(
			"unexpected proto type: %T, want "+
				"*roundpb.JoinRoundRequest", p,
		)
	}

	// Parse identifier public key.
	if len(pb.Identifier) > 0 {
		key, err := btcec.ParsePubKey(pb.Identifier)
		if err != nil {
			return fmt.Errorf("identifier: %w", err)
		}
		m.Identifier = key
	}

	// Convert boarding requests.
	m.BoardingRequests = make(
		[]types.BoardingRequest, len(pb.BoardingRequests),
	)
	for i, br := range pb.BoardingRequests {
		req := types.BoardingRequest{
			ExitDelay: br.ExitDelay,
		}

		if br.Outpoint != nil {
			op, err := roundpb.OutpointFromProto(
				br.Outpoint,
			)
			if err != nil {
				return fmt.Errorf(
					"boarding_requests[%d].outpoint: %w",
					i, err,
				)
			}
			req.Outpoint = &op
		}

		if len(br.ClientKey) > 0 {
			key, err := btcec.ParsePubKey(br.ClientKey)
			if err != nil {
				return fmt.Errorf(
					"boarding_requests[%d].client_key: %w",
					i, err,
				)
			}
			req.ClientKey = key
		}

		if len(br.OperatorKey) > 0 {
			key, err := btcec.ParsePubKey(br.OperatorKey)
			if err != nil {
				return fmt.Errorf(
					"boarding[%d].operator_key: %w",
					i, err,
				)
			}
			req.OperatorKey = key
		}

		m.BoardingRequests[i] = req
	}

	// Convert VTXO requests. Server-originated messages don't carry
	// signing keys — those are derived locally by the FSM.
	m.VTXORequests = make(
		[]RoundVTXORequest, len(pb.VtxoRequests),
	)
	for i, vr := range pb.VtxoRequests {
		req := types.VTXORequest{
			Amount:   btcutil.Amount(vr.Amount),
			PkScript: vr.PkScript,
			Expiry:   vr.Expiry,
		}

		if len(vr.ClientKey) > 0 {
			key, err := btcec.ParsePubKey(vr.ClientKey)
			if err != nil {
				return fmt.Errorf(
					"vtxo_requests[%d].client_key: %w",
					i, err,
				)
			}
			req.OwnerKey = keychain.KeyDescriptor{
				PubKey: key,
			}
		}

		if len(vr.OperatorKey) > 0 {
			key, err := btcec.ParsePubKey(vr.OperatorKey)
			if err != nil {
				return fmt.Errorf(
					"vtxo_requests[%d].operator_key: %w",
					i, err,
				)
			}
			req.OperatorKey = key
		}

		roundReq := RoundVTXORequest{
			VTXOIntent: vtxoRequestToIntent(req),
		}

		// Parse the signing key when present. Server-originated
		// messages may omit it, but client round-trip decoding
		// must preserve it.
		if len(vr.SigningKey) > 0 {
			key, err := btcec.ParsePubKey(vr.SigningKey)
			if err != nil {
				return fmt.Errorf(
					"vtxo_requests[%d].signing_key: %w",
					i, err,
				)
			}
			roundReq.SigningKey = keychain.KeyDescriptor{
				PubKey: key,
			}
		}

		m.VTXORequests[i] = roundReq
	}

	// Convert forfeit requests.
	m.ForfeitRequests = make(
		[]*types.ForfeitRequest, len(pb.ForfeitRequests),
	)
	for i, fr := range pb.ForfeitRequests {
		req := &types.ForfeitRequest{}

		if fr.VtxoOutpoint != nil {
			op, err := roundpb.OutpointFromProto(
				fr.VtxoOutpoint,
			)
			if err != nil {
				return fmt.Errorf(
					"forfeit[%d].vtxo_outpoint: %w",
					i, err,
				)
			}
			req.VTXOOutpoint = &op
		}

		m.ForfeitRequests[i] = req
	}

	// Convert leave requests.
	m.LeaveRequests = make(
		[]*types.LeaveRequest, len(pb.LeaveRequests),
	)
	for i, lr := range pb.LeaveRequests {
		req := &types.LeaveRequest{}

		if lr.Output != nil {
			out, outErr := roundpb.TxOutFromProto(
				lr.Output,
			)
			if outErr != nil {
				return fmt.Errorf(
					"leave_requests[%d].output: %w",
					i, outErr,
				)
			}
			req.Output = out
		}

		m.LeaveRequests[i] = req
	}

	m.RoundID = pb.RoundId

	// Convert auth payload.
	if pb.Auth != nil {
		m.Auth = &types.JoinRoundAuth{
			Message:    pb.Auth.Message,
			ValidFrom:  pb.Auth.ValidFrom,
			ValidUntil: pb.Auth.ValidUntil,
			Signature:  pb.Auth.Signature,
		}
	}

	return nil
}

// Compile-time assertions ensuring all event types that arrive from the
// server implement the InboundServerMessage interface for automatic proto
// conversion by the serverconn event router.
var (
	_ inboundServerMessage = (*RoundJoined)(nil)
	_ inboundServerMessage = (*CommitmentTxBuilt)(nil)
	_ inboundServerMessage = (*AwaitingBoardingSigs)(nil)
	_ inboundServerMessage = (*NoncesAggregated)(nil)
	_ inboundServerMessage = (*OperatorSigned)(nil)
	_ inboundServerMessage = (*BoardingFailed)(nil)
)

// inboundServerMessage mirrors the serverconn.InboundServerMessage interface
// locally to avoid an import cycle between round and serverconn packages.
type inboundServerMessage interface {
	FromProto(proto.Message) error
}
