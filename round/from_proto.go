package round

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"google.golang.org/protobuf/proto"
)

// MaxQuoteEntriesPerClient bounds the per-quote VTXO / leave entry
// slices the client is willing to decode from the server. The
// realistic upper bound for a single client's intent is well under
// a hundred entries (boarding rounds: tens of recipients; refresh:
// bounded by select-limit); 1024 is a generous cap that lets
// FromProto cheaply reject malformed or malicious envelopes before
// allocating large backing slices.
const MaxQuoteEntriesPerClient = 1024

// FromProto populates a JoinRoundQuoteReceived from a
// roundpb.JoinRoundQuote proto. The round_id is parsed as a UUID
// string (matching the server's envelope encoding), the quote_id is
// narrowed to a 32-byte fixed array, and the VTXO / Leave amount
// slices are indexed positionally against the client's intent.
//
// The FSM consumes this event in IntentSentState to transition into
// QuoteReceivedState, where env.MaxOperatorFee is checked against
// OperatorFeeSat and accept / reject decisions are made.
func (e *JoinRoundQuoteReceived) FromProto(p proto.Message) error {
	pb, ok := p.(*roundpb.JoinRoundQuote)
	if !ok {
		return fmt.Errorf("unexpected proto type: %T, want "+
			"*roundpb.JoinRoundQuote", p)
	}

	// Parse round ID from the UUID-canonical string.
	roundID, err := parseRoundIDString(pb.GetRoundId())
	if err != nil {
		return fmt.Errorf("round_id: %w", err)
	}
	e.RoundID = roundID

	if len(pb.GetQuoteId()) != 32 {
		return fmt.Errorf("invalid quote_id length: %d, want 32",
			len(pb.GetQuoteId()))
	}
	var quoteID [32]byte
	copy(quoteID[:], pb.GetQuoteId())

	rawReason := pb.GetRejectReason()
	if _, known := roundpb.QuoteReason_name[int32(rawReason)]; !known {
		return fmt.Errorf("unknown reject_reason value: %d", rawReason)
	}

	quote := &ClientQuote{
		QuoteID:        quoteID,
		SealPass:       pb.GetSealPassNumber(),
		OperatorFeeSat: pb.GetOperatorFeeSat(),
		QuoteExpiresAt: pb.GetQuoteExpiresAt(),
		RejectReason:   rawReason,
	}

	vtxoQuotes := pb.GetVtxoQuotes()
	if len(vtxoQuotes) > MaxQuoteEntriesPerClient {
		return fmt.Errorf("vtxo_quotes length %d exceeds cap %d",
			len(vtxoQuotes), MaxQuoteEntriesPerClient)
	}
	if len(vtxoQuotes) > 0 {
		quote.VTXOQuotes = make([]VTXOQuoteEntry, 0, len(vtxoQuotes))
		for i, vq := range vtxoQuotes {
			amt := vq.GetAmountSat()
			if amt < 0 {
				return fmt.Errorf("vtxo_quotes[%d].amount_sat "+
					"is negative: %d", i, amt)
			}

			quote.VTXOQuotes = append(
				quote.VTXOQuotes, VTXOQuoteEntry{
					PkScript:     vq.GetPkScript(),
					AmountSat:    amt,
					RecipientKey: vq.GetRecipientKey(),
				},
			)
		}
	}

	leaveQuotes := pb.GetLeaveQuotes()
	if len(leaveQuotes) > MaxQuoteEntriesPerClient {
		return fmt.Errorf("leave_quotes length %d exceeds cap %d",
			len(leaveQuotes), MaxQuoteEntriesPerClient)
	}
	if len(leaveQuotes) > 0 {
		quote.LeaveQuotes = make([]LeaveQuoteEntry, 0, len(leaveQuotes))
		for i, lq := range leaveQuotes {
			amt := lq.GetAmountSat()
			if amt < 0 {
				return fmt.Errorf(
					"leave_quotes[%d].amount_sat is "+
						"negative: %d", i, amt)
			}

			quote.LeaveQuotes = append(
				quote.LeaveQuotes, LeaveQuoteEntry{
					PkScript:  lq.GetPkScript(),
					AmountSat: amt,
				},
			)
		}
	}

	claimQuotes := pb.GetClaimQuotes()
	if len(claimQuotes) > MaxQuoteEntriesPerClient {
		return fmt.Errorf("claim_quotes length %d exceeds cap %d",
			len(claimQuotes), MaxQuoteEntriesPerClient)
	}
	if len(claimQuotes) > 0 {
		quote.ClaimQuotes = make(
			[]VTXOClaimQuoteEntry, 0, len(claimQuotes),
		)
		for i, cq := range claimQuotes {
			if cq.GetSourceOutpoint() == nil {
				return fmt.Errorf(
					"claim_quotes[%d].source_outpoint is "+
						"required", i)
			}
			source, err := roundpb.OutpointFromProto(
				cq.GetSourceOutpoint(),
			)
			if err != nil {
				return fmt.Errorf(
					"claim_quotes[%d].source_outpoint: %w",
					i, err)
			}
			amt := cq.GetAmountSat()
			if amt < 0 {
				return fmt.Errorf(
					"claim_quotes[%d].amount_sat is "+
						"negative: %d", i, amt)
			}
			replacementSigningKey :=
				cq.GetReplacementSigningPubkey()

			quote.ClaimQuotes = append(
				quote.ClaimQuotes, VTXOClaimQuoteEntry{
					SourceOutpoint: source,
					PkScript: bytes.Clone(
						cq.GetPkScript(),
					),
					PolicyTemplate: bytes.Clone(
						cq.GetPolicyTemplate(),
					),
					AmountSat: amt,
					ReplacementSigningKey: bytes.Clone(
						replacementSigningKey,
					),
				},
			)
		}
	}

	e.Quote = quote

	return nil
}

// parseRoundIDString parses the canonical UUID string used on the
// JoinRoundQuote wire envelope into a RoundID. Kept private because
// the client otherwise deals with raw 16-byte round IDs.
func parseRoundIDString(s string) (RoundID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return RoundID{}, fmt.Errorf("invalid round_id %q: %w", s, err)
	}

	return RoundID(u), nil
}

// FromProto populates a RoundJoined from a ClientSuccessResp proto. The round
// ID is parsed as a 16-byte UUID and outpoints are converted from their proto
// representation.
func (e *RoundJoined) FromProto(p proto.Message) error {
	pb, ok := p.(*roundpb.ClientSuccessResp)
	if !ok {
		return fmt.Errorf("unexpected proto type: %T, want "+
			"*roundpb.ClientSuccessResp", p)
	}

	// Parse round ID from 16-byte UUID.
	if len(pb.RoundId) != 16 {
		return fmt.Errorf("invalid round_id length: %d",
			len(pb.RoundId))
	}
	copy(e.RoundID[:], pb.RoundId)

	// Convert accepted boarding outpoints.
	boardingOps, err := roundpb.OutpointsFromProto(
		pb.AcceptedBoardingOutpoints,
	)
	if err != nil {
		return fmt.Errorf("accepted_boarding_outpoints: %w", err)
	}
	e.AcceptedBoardingOutpoints = boardingOps

	// Convert accepted VTXO outpoints.
	vtxoOps, err := roundpb.OutpointsFromProto(
		pb.AcceptedVtxoOutpoints,
	)
	if err != nil {
		return fmt.Errorf("accepted_vtxo_outpoints: %w", err)
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
		return fmt.Errorf("unexpected proto type: %T, want "+
			"*roundpb.ClientBatchInfo", p)
	}

	// Parse round ID.
	if len(pb.RoundId) != 16 {
		return fmt.Errorf("invalid round_id length: %d",
			len(pb.RoundId))
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
				return fmt.Errorf("vtxo_tree_paths: negative "+
					"index %d", idx)
			}

			t, treeErr := roundpb.TreeFromProto(
				pt, e.TreeOpts...,
			)
			if treeErr != nil {
				return fmt.Errorf("vtxo_tree_paths[%d]: %w",
					idx, treeErr)
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
				return fmt.Errorf("connector_leaf_map key "+
					"%q: %w", key, opErr)
			}

			connOP, connErr := roundpb.OutpointFromProto(
				info.LeafOutpoint,
			)
			if connErr != nil {
				return fmt.Errorf("connector_leaf_map[%s] "+
					"leaf_outpoint: %w", key, connErr)
			}

			leafOut, leafErr := roundpb.TxOutFromProto(
				info.LeafOutput,
			)
			if leafErr != nil {
				return fmt.Errorf("connector_leaf_map[%s] "+
					"leaf_output: %w", key, leafErr)
			}
			if leafOut == nil {
				return fmt.Errorf("connector_leaf_map[%s] nil "+
					"leaf_output", key)
			}

			e.ForfeitMappings[op] = &ConnectorLeafInfo{
				ConnectorOutpoint: connOP,
				ConnectorPkScript: leafOut.PkScript,
				ConnectorAmount:   leafOut.Value,
				RootOutputIndex:   info.GetRootOutputIndex(),
				NumLeaves:         info.GetNumLeaves(),
				Radix:             info.GetRadix(),
				LeafIndex:         int(info.GetLeafIndex()),
			}
		}
	}

	// Parse this round's signing keys when present. A server that predates
	// these fields leaves them empty; the FSM then falls back to the global
	// operator key for tree validation and connector reconstruction.
	if len(pb.TreeCosignKey) > 0 {
		key, keyErr := btcec.ParsePubKey(pb.TreeCosignKey)
		if keyErr != nil {
			return fmt.Errorf("tree_cosign_key: %w", keyErr)
		}
		e.TreeCosignKey = key
	}
	if len(pb.ConnectorOperatorKey) > 0 {
		key, keyErr := btcec.ParsePubKey(pb.ConnectorOperatorKey)
		if keyErr != nil {
			return fmt.Errorf("connector_operator_key: %w", keyErr)
		}
		e.ConnectorOperatorKey = key
	}

	// Parse this round's sweep key and delay. These are delivered per round
	// (replacing the removed global GetInfo sweep terms), so the FSM uses
	// them to validate the VTXO-tree sweep branch and compute batch expiry.
	if len(pb.SweepKey) > 0 {
		key, keyErr := btcec.ParsePubKey(pb.SweepKey)
		if keyErr != nil {
			return fmt.Errorf("sweep_key: %w", keyErr)
		}
		e.SweepKey = key
	}
	e.SweepDelay = pb.SweepDelay

	// Parse this round's forfeit penalty key. It is delivered per round
	// (replacing the removed global GetInfo forfeit script), so the FSM
	// derives the forfeit-tx penalty output script from it.
	if len(pb.ForfeitKey) > 0 {
		key, keyErr := btcec.ParsePubKey(pb.ForfeitKey)
		if keyErr != nil {
			return fmt.Errorf("forfeit_key: %w", keyErr)
		}
		e.ForfeitKey = key
	}

	// Record and validate the round flow version stamped by the operator.
	// Fail closed on a version this build does not understand rather than
	// joining a round conducted under unknown choreography rules. Versions
	// are zero-indexed, so an omitted wire field reads as V1.
	if err := roundpb.ValidateFlowVersion(
		roundpb.FlowVersion(
			pb.GetFlowVersion(),
		),
	); err != nil {
		return err
	}
	e.FlowVersion = roundpb.FlowVersion(pb.GetFlowVersion())

	return nil
}

// FromProto populates an AwaitingBoardingSigs from a
// ClientAwaitingInputSigsResp proto.
func (e *AwaitingBoardingSigs) FromProto(p proto.Message) error {
	pb, ok := p.(*roundpb.ClientAwaitingInputSigsResp)
	if !ok {
		return fmt.Errorf("unexpected proto type: %T, want "+
			"*roundpb.ClientAwaitingInputSigsResp", p)
	}

	if len(pb.RoundId) != 16 {
		return fmt.Errorf("invalid round_id length: %d",
			len(pb.RoundId))
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
		return fmt.Errorf("unexpected proto type: %T, want "+
			"*roundpb.ClientVTXOAggNonces", p)
	}

	if len(pb.RoundId) != 16 {
		return fmt.Errorf("invalid round_id length: %d",
			len(pb.RoundId))
	}
	copy(e.RoundID[:], pb.RoundId)

	// Convert aggregated nonces map. Keys are hex-encoded TxIDs,
	// values are 66-byte MuSig2 public nonces.
	if pb.AggNonces != nil {
		e.AggNonces = make(
			map[tree.TxID]tree.Musig2PubNonce, len(pb.AggNonces),
		)
		for hexID, nonceBytes := range pb.AggNonces {
			txID, err := roundpb.TxIDFromHex(hexID)
			if err != nil {
				return fmt.Errorf("agg_nonces key: %w", err)
			}

			var expected tree.Musig2PubNonce
			if len(nonceBytes) != len(expected) {
				return fmt.Errorf("agg_nonces[%s] invalid "+
					"nonce length: %d", hexID,
					len(nonceBytes))
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
		return fmt.Errorf("unexpected proto type: %T, want "+
			"*roundpb.ClientVTXOAggSigs", p)
	}

	if len(pb.RoundId) != 16 {
		return fmt.Errorf("invalid round_id length: %d",
			len(pb.RoundId))
	}
	copy(e.RoundID[:], pb.RoundId)

	// Convert aggregated signatures map. Keys are hex-encoded TxIDs,
	// values are 64-byte schnorr signatures.
	if pb.AggSigs != nil {
		e.AggSigs = make(
			map[tree.TxID]*schnorr.Signature, len(pb.AggSigs),
		)
		for hexID, sigBytes := range pb.AggSigs {
			txID, err := roundpb.TxIDFromHex(hexID)
			if err != nil {
				return fmt.Errorf("agg_sigs key: %w", err)
			}

			sig, sigErr := roundpb.SchnorrSigFromBytes(
				sigBytes,
			)
			if sigErr != nil {
				return fmt.Errorf("agg_sigs[%s]: %w", hexID,
					sigErr)
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

		// Recoverable and FailureCode are orthogonal axes.
		// Recoverable is round-level: this round is over, so its inputs
		// return to the live set. FailureCode is job-level: a terminal
		// code (e.g. insufficient operator funds) tells the actor the
		// originating job is dead and must not replay, even though the
		// round is recoverable. So a terminal failure is still
		// Recoverable = true here; do not conflate the two.
		e.Recoverable = true
		e.FailureCode = failureCodeFromProto(pb.FailureCode)

		// Carry the server-assigned round id when present so the actor
		// can route this failure deterministically to the matching FSM.
		// A failure that arrives before the round was assigned
		// legitimately has no id (len != roundIDLen), in which case
		// RoundID stays None and routing falls back to the sole-round
		// heuristic.
		if len(pb.RoundId) == roundIDLen {
			var rid RoundID
			copy(rid[:], pb.RoundId)
			e.RoundID = fn.Some(rid)
		}

		return nil

	case *roundpb.ClientErrorResp:
		// ClientErrorResp is also mapped to BoardingFailed, matching
		// the bridge's convertToClientEvent behavior.
		e.Reason = pb.ErrorMsg
		e.Recoverable = true

		return nil

	default:
		return fmt.Errorf("unexpected proto type: %T, want "+
			"*roundpb.ClientRoundFailedResp or "+
			"*roundpb.ClientErrorResp", p)
	}
}

// failureCodeFromProto maps the wire roundpb.RoundFailureCode onto the native
// RoundFailureCode. This is the RPC boundary; an unrecognized wire code (a
// newer server than this client) degrades to RoundFailureUnknown so the client
// falls back to recoverable handling and the reason string.
func failureCodeFromProto(code roundpb.RoundFailureCode) RoundFailureCode {
	switch code {
	case roundpb.RoundFailureCode_ROUND_FAILURE_INSUFFICIENT_OPERATOR_FUNDS:
		return RoundFailureInsufficientOperatorFunds

	default:
		return RoundFailureUnknown
	}
}

// FromProto populates a JoinRoundRequest from its proto representation. This
// is the inverse of ToProto and is used by test code that needs to
// deserialize captured mailbox envelopes back into domain types.
func (m *JoinRoundRequest) FromProto(p proto.Message) error {
	pb, ok := p.(*roundpb.JoinRoundRequest)
	if !ok {
		return fmt.Errorf("unexpected proto type: %T, want "+
			"*roundpb.JoinRoundRequest", p)
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
			PolicyTemplate: bytes.Clone(br.PolicyTemplate),
		}

		if br.Outpoint != nil {
			op, err := roundpb.OutpointFromProto(
				br.Outpoint,
			)
			if err != nil {
				return fmt.Errorf(
					"boarding_requests[%d].outpoint: %w", i,
					err)
			}
			req.Outpoint = &op
		}

		_, err := req.DecodePolicyTemplate()
		if err != nil {
			return fmt.Errorf(
				"boarding_requests[%d].policy_template: %w", i,
				err)
		}

		m.BoardingRequests[i] = req
	}

	// Convert VTXO requests.
	m.VTXORequests = make(
		[]types.VTXORequest, len(pb.VtxoRequests),
	)
	for i, vr := range pb.VtxoRequests {
		req := types.VTXORequest{
			Amount:         btcutil.Amount(vr.TargetAmountSat),
			IsChange:       vr.IsChange,
			FixedAmount:    vr.FixedAmount,
			PolicyTemplate: bytes.Clone(vr.PolicyTemplate),
		}

		if len(vr.SigningKey) > 0 {
			key, err := btcec.ParsePubKey(vr.SigningKey)
			if err != nil {
				return fmt.Errorf(
					"vtxo_requests[%d].signing_key: %w", i,
					err)
			}

			req.SigningKey = keychain.KeyDescriptor{
				PubKey: key,
			}
		}

		_, err := req.DecodePolicyTemplate()
		if err != nil {
			return fmt.Errorf(
				"vtxo_requests[%d].policy_template: %w", i, err)
		}

		m.VTXORequests[i] = req
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
					"forfeit[%d].vtxo_outpoint: %w", i, err)
			}
			req.VTXOOutpoint = &op
		}
		if len(fr.AuthSpendPath) > 0 {
			spend, err := arkscript.DecodeSpendPath(
				fr.AuthSpendPath,
			)
			if err != nil {
				return fmt.Errorf(
					"forfeit[%d].auth_spend_path: %w", i,
					err)
			}
			req.AuthSpend = spend
		}
		if len(fr.ForfeitSpendPath) > 0 {
			spend, err := arkscript.DecodeSpendPath(
				fr.ForfeitSpendPath,
			)
			if err != nil {
				return fmt.Errorf(
					"forfeit[%d].forfeit_spend_path: %w", i,
					err)
			}
			req.ForfeitSpend = spend
		}

		m.ForfeitRequests[i] = req
	}

	// Convert leave requests.
	m.LeaveRequests = make(
		[]*types.LeaveRequest, len(pb.LeaveRequests),
	)
	for i, lr := range pb.LeaveRequests {
		req := &types.LeaveRequest{
			Output: &wire.TxOut{
				Value:    lr.TargetAmountSat,
				PkScript: bytes.Clone(lr.PkScript),
			},
			IsChange: lr.IsChange,
		}

		m.LeaveRequests[i] = req
	}

	// Convert independently signed expired-VTXO claims.
	m.ClaimInputs = make(
		[]*types.VTXOClaimInput, len(pb.ClaimInputs),
	)
	for i, ci := range pb.ClaimInputs {
		if ci == nil || ci.SourceOutpoint == nil {
			return fmt.Errorf("claim_inputs[%d].source_outpoint "+
				"is required", i)
		}

		source, err := roundpb.OutpointFromProto(ci.SourceOutpoint)
		if err != nil {
			return fmt.Errorf(
				"claim_inputs[%d].source_outpoint: %w", i, err)
		}
		participant, err := btcec.ParsePubKey(ci.ParticipantPubkey)
		if err != nil {
			return fmt.Errorf(
				"claim_inputs[%d].participant_pubkey: %w", i,
				err)
		}
		replacement, err := btcec.ParsePubKey(
			ci.ReplacementSigningPubkey,
		)
		if err != nil {
			return fmt.Errorf(
				"claim_inputs[%d].replacement_signing_pubkey:"+
					" %w", i, err)
		}
		if len(ci.Nonce) != types.VTXOClaimNonceSize {
			return fmt.Errorf("claim_inputs[%d].nonce length "+
				"%d, want %d", i, len(ci.Nonce),
				types.VTXOClaimNonceSize)
		}
		if len(ci.Signature) != types.VTXOClaimSignatureSize {
			return fmt.Errorf("claim_inputs[%d].signature length "+
				"%d, want %d", i, len(ci.Signature),
				types.VTXOClaimSignatureSize)
		}

		claim := &types.VTXOClaimInput{
			SourceOutpoint:    source,
			ParticipantPubKey: participant,
			ReplacementSigningKey: keychain.KeyDescriptor{
				PubKey: replacement,
			},
			ValidFrom:  ci.ValidFrom,
			ValidUntil: ci.ValidUntil,
			Signature:  bytes.Clone(ci.Signature),
		}
		copy(claim.Nonce[:], ci.Nonce)
		m.ClaimInputs[i] = claim
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
	_ inboundServerMessage = (*JoinRoundQuoteReceived)(nil)
)

// inboundServerMessage mirrors the serverconn.InboundServerMessage interface
// locally to avoid an import cycle between round and serverconn packages.
type inboundServerMessage interface {
	FromProto(proto.Message) error
}

// FromProto populates a RoundStatusReported from a ClientRoundStatusReport
// proto. The round id is required: a report that cannot be tied to a round
// is rejected rather than routed by heuristic, because the consumer uses
// the answer to decide whether releasing forfeit reservations is safe.
func (e *RoundStatusReported) FromProto(p proto.Message) error {
	pb, ok := p.(*roundpb.ClientRoundStatusReport)
	if !ok {
		return fmt.Errorf("unexpected proto type: %T, want "+
			"*roundpb.ClientRoundStatusReport", p)
	}

	if len(pb.RoundId) != roundIDLen {
		return fmt.Errorf("round status report round_id must be %d "+
			"bytes, got %d", roundIDLen, len(pb.RoundId))
	}

	copy(e.RoundID[:], pb.RoundId)
	e.Status = pb.Status
	e.Detail = pb.Detail

	return nil
}
