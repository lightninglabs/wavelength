package oorpb

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
)

const (
	// ServiceName is the mailbox RPC service name for client/server OOR
	// submit/finalize request-response flows.
	ServiceName = "oorpb.OORMailboxService"

	// MethodSubmitPackage maps a client submit-package request.
	MethodSubmitPackage = "SubmitPackage"

	// MethodFinalizePackage maps a client finalize-package request.
	MethodFinalizePackage = "FinalizePackage"

	// MethodIncomingAck maps a client incoming-transfer acknowledgment.
	MethodIncomingAck = "IncomingAck"
)

// SigningDescriptor is the minimal signing metadata needed by the server OOR
// actor to co-sign checkpoint inputs.
type SigningDescriptor struct {
	Outpoint wire.OutPoint

	// VTXOPolicyTemplate is the serialized arkscript policy for the spent
	// input VTXO.
	VTXOPolicyTemplate []byte

	// SpendPath is the serialized arkscript spend path selected for the
	// checkpoint spend of the input VTXO.
	SpendPath []byte

	// OwnerLeafPolicy is the serialized arkscript owner-leaf policy
	// for the checkpoint output created from this input.
	OwnerLeafPolicy []byte
}

// NewSubmitPackageRequest builds a typed proto request for SubmitPackage.
func NewSubmitPackageRequest(ark *psbt.Packet, checkpoints []*psbt.Packet,
	descs []SigningDescriptor,
	recipients []oortx.RecipientOutput) (*SubmitPackageRequest, error) {

	arkRaw, err := psbtutil.Serialize(ark)
	if err != nil {
		return nil, err
	}

	checkpointRaw, err := encodePSBTSlice(checkpoints)
	if err != nil {
		return nil, err
	}

	protoDescs := make([]*OORSigningDescriptor, 0, len(descs))
	for i := range descs {
		desc, err := encodeSigningDescriptor(descs[i], i)
		if err != nil {
			return nil, err
		}

		protoDescs = append(protoDescs, desc)
	}

	protoRecipients := make(
		[]*OORRecipientOutput, 0, len(recipients),
	)
	for i := range recipients {
		protoRecipients = append(
			protoRecipients, &OORRecipientOutput{
				PkScript: recipients[i].PkScript,
				ValueSat: int64(recipients[i].Value),
				VtxoPolicyTemplate: recipients[i].
					VTXOPolicyTemplate,
			},
		)
	}

	return &SubmitPackageRequest{
		ArkPsbt:            arkRaw,
		CheckpointPsbts:    checkpointRaw,
		SigningDescriptors: protoDescs,
		RecipientOutputs:   protoRecipients,
	}, nil
}

// ParseSubmitPackageRequest decodes a SubmitPackageRequest into domain types.
func ParseSubmitPackageRequest(req *SubmitPackageRequest) (*psbt.Packet,
	[]*psbt.Packet, []SigningDescriptor, []oortx.RecipientOutput, error) {

	if req == nil {
		return nil, nil, nil, nil,
			fmt.Errorf("submit request is required")
	}

	ark, err := psbtutil.Parse(req.ArkPsbt)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	checkpoints, err := decodePSBTSlice(req.CheckpointPsbts)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	descs := make([]SigningDescriptor, 0, len(req.SigningDescriptors))
	for i := range req.SigningDescriptors {
		desc, err := decodeSigningDescriptor(
			req.SigningDescriptors[i], i,
		)
		if err != nil {
			return nil, nil, nil, nil, err
		}

		descs = append(descs, desc)
	}

	recipients := make(
		[]oortx.RecipientOutput, 0, len(req.RecipientOutputs),
	)
	for i := range req.RecipientOutputs {
		recipient := req.RecipientOutputs[i]
		if recipient == nil {
			return nil, nil, nil, nil, fmt.Errorf("recipient "+
				"output %d is nil", i)
		}

		recipients = append(recipients, oortx.RecipientOutput{
			PkScript: recipient.PkScript,
			Value:    btcutil.Amount(recipient.ValueSat),
			VTXOPolicyTemplate: recipient.
				VtxoPolicyTemplate,
		})
	}

	return ark, checkpoints, descs, recipients, nil
}

// NewSubmitPackageResponse builds a typed proto response for SubmitPackage's
// success branch. Operator-side rejections use NewSubmitPackageRejection.
//
// coSignedArk is the exact Ark PSBT after the operator attaches its signature
// material. The client must persist this artifact rather than rebuilding it
// from local state so later unilateral OOR recovery can broadcast the proof
// transaction without asking the operator for another signature.
func NewSubmitPackageResponse(sessionID chainhash.Hash,
	coSignedArk *psbt.Packet,
	coSignedCheckpoints []*psbt.Packet) (*SubmitPackageResponse, error) {

	arkRaw, err := psbtutil.Serialize(coSignedArk)
	if err != nil {
		return nil, err
	}

	checkpointRaw, err := encodePSBTSlice(coSignedCheckpoints)
	if err != nil {
		return nil, err
	}

	return &SubmitPackageResponse{
		Result: &SubmitPackageResponse_Success{
			Success: &SubmitPackageSuccess{
				SessionId:               sessionID.CloneBytes(),
				CoSignedCheckpointPsbts: checkpointRaw,
				CoSignedArkPsbt:         arkRaw,
			},
		},
	}, nil
}

// NewSubmitPackageRejection builds a typed proto rejection branch of
// SubmitPackageResponse. The code lets clients route on the cause
// without string-matching the reason; reason carries a human-readable
// explanation suitable for logs and UX surfaces; sessionID echoes the
// rejected submit's session hash so the client-side EventRouter can
// route the failure to the correct OOR session FSM rather than
// stalling the ingress cursor on an undispatchable envelope.
func NewSubmitPackageRejection(sessionID chainhash.Hash, code OORRejectCode,
	reason string) *SubmitPackageResponse {

	return &SubmitPackageResponse{
		Result: &SubmitPackageResponse_Rejection{
			Rejection: &SubmitPackageRejection{
				Code:      code,
				Reason:    reason,
				SessionId: sessionID.CloneBytes(),
			},
		},
	}
}

// ParseSubmitPackageResponse decodes the success branch of a
// SubmitPackageResponse. Returns ErrSubmitPackageRejected when the response
// carries a rejection branch; callers can recover the typed code and
// reason via errors.As.
func ParseSubmitPackageResponse(resp *SubmitPackageResponse) (chainhash.Hash,
	*psbt.Packet, []*psbt.Packet, error) {

	if resp == nil {
		return chainhash.Hash{}, nil, nil,
			fmt.Errorf("submit response is required")
	}

	switch r := resp.Result.(type) {
	case *SubmitPackageResponse_Success:
		if r.Success == nil {
			return chainhash.Hash{}, nil, nil,
				fmt.Errorf("submit success branch is empty")
		}

		sessionID, err := decodeSessionID(r.Success.SessionId)
		if err != nil {
			return chainhash.Hash{}, nil, nil, err
		}

		// co_signed_ark_psbt is an additive field introduced after
		// the initial submit-package wire shape: operators that have
		// not been upgraded yet still return success without
		// populating it. Treat empty bytes as "operator did not
		// include the artifact" rather than as a parse error so
		// clients can keep talking to older operators during a
		// rolling upgrade. Recovery flows that genuinely need the
		// co-signed PSBT will surface the absence at the recovery
		// boundary, not here on every submit.
		var coSignedArk *psbt.Packet
		if len(r.Success.CoSignedArkPsbt) > 0 {
			coSignedArk, err = psbtutil.Parse(
				r.Success.CoSignedArkPsbt,
			)
			if err != nil {
				return chainhash.Hash{}, nil, nil,
					fmt.Errorf("decode co-signed ark "+
						"psbt: %w", err)
			}
		}

		checkpoints, err := decodePSBTSlice(
			r.Success.CoSignedCheckpointPsbts,
		)
		if err != nil {
			return chainhash.Hash{}, nil, nil, err
		}

		return sessionID, coSignedArk, checkpoints, nil

	case *SubmitPackageResponse_Rejection:
		if r.Rejection == nil {
			return chainhash.Hash{}, nil, nil,
				fmt.Errorf("submit rejection branch is empty")
		}

		// The rejection echoes session_id so the durable EventRouter
		// dispatch path can route the failure to the correct OOR
		// session FSM rather than stalling the ingress cursor on an
		// undispatchable envelope. Decode it best-effort: a malformed
		// session_id still surfaces as a typed error but with a zero
		// hash, and the FSM-side OutboxErrorEvent path treats zero as
		// a non-routable rejection.
		sessionID, err := decodeSessionID(r.Rejection.SessionId)
		if err != nil {
			return chainhash.Hash{}, nil, nil, fmt.Errorf("decode "+
				"rejected session id: %w", err)
		}

		return sessionID, nil, nil, &SubmitRejectedError{
			Code:   r.Rejection.Code,
			Reason: r.Rejection.Reason,
		}

	default:
		return chainhash.Hash{}, nil, nil,
			fmt.Errorf("submit response carries no result branch")
	}
}

// SubmitRejectedError is returned by ParseSubmitPackageResponse when the
// operator rejected the submit with a typed code. Callers route on Code
// (e.g. fall back to in-round payment for OOR_REJECT_LINEAGE_TOO_LARGE)
// without string-matching Reason.
type SubmitRejectedError struct {
	Code   OORRejectCode
	Reason string
}

// Error reports the typed rejection in a human-readable form.
func (e *SubmitRejectedError) Error() string {
	return fmt.Sprintf("oor submit rejected (%s): %s", e.Code, e.Reason)
}

// NewFinalizePackageRequest builds a typed proto request for FinalizePackage.
func NewFinalizePackageRequest(sessionID chainhash.Hash,
	finalCheckpoints []*psbt.Packet) (*FinalizePackageRequest, error) {

	checkpointRaw, err := encodePSBTSlice(finalCheckpoints)
	if err != nil {
		return nil, err
	}

	return &FinalizePackageRequest{
		SessionId:            sessionID.CloneBytes(),
		FinalCheckpointPsbts: checkpointRaw,
	}, nil
}

// ParseFinalizePackageRequest decodes a FinalizePackageRequest.
func ParseFinalizePackageRequest(req *FinalizePackageRequest) (chainhash.Hash,
	[]*psbt.Packet, error) {

	if req == nil {
		return chainhash.Hash{}, nil,
			fmt.Errorf("finalize request is required")
	}

	sessionID, err := decodeSessionID(req.SessionId)
	if err != nil {
		return chainhash.Hash{}, nil, err
	}

	finalCheckpoints, err := decodePSBTSlice(req.FinalCheckpointPsbts)
	if err != nil {
		return chainhash.Hash{}, nil, err
	}

	return sessionID, finalCheckpoints, nil
}

// NewFinalizePackageResponse builds a typed proto response for FinalizePackage.
func NewFinalizePackageResponse(
	sessionID chainhash.Hash,
) *FinalizePackageResponse {

	return &FinalizePackageResponse{
		SessionId: sessionID.CloneBytes(),
	}
}

// ParseFinalizePackageResponse decodes a FinalizePackageResponse.
func ParseFinalizePackageResponse(resp *FinalizePackageResponse) (
	chainhash.Hash, error) {

	if resp == nil {
		return chainhash.Hash{},
			fmt.Errorf("finalize response is required")
	}

	return decodeSessionID(resp.SessionId)
}

// encodeSigningDescriptor converts one descriptor to proto form.
func encodeSigningDescriptor(desc SigningDescriptor,
	index int) (*OORSigningDescriptor, error) {

	proto := &OORSigningDescriptor{
		Outpoint:           encodeOutPoint(desc.Outpoint),
		VtxoPolicyTemplate: desc.VTXOPolicyTemplate,
		SpendPath:          desc.SpendPath,
		OwnerLeafPolicy:    desc.OwnerLeafPolicy,
	}

	return proto, nil
}

// decodeSigningDescriptor converts one proto descriptor to domain form.
func decodeSigningDescriptor(desc *OORSigningDescriptor,
	index int) (SigningDescriptor, error) {

	if desc == nil {
		return SigningDescriptor{}, fmt.Errorf("signing descriptor "+
			"%d is nil", index)
	}

	outpoint, err := decodeOutPoint(desc.Outpoint)
	if err != nil {
		return SigningDescriptor{}, err
	}

	result := SigningDescriptor{
		Outpoint:           outpoint,
		VTXOPolicyTemplate: desc.VtxoPolicyTemplate,
		SpendPath:          desc.SpendPath,
		OwnerLeafPolicy:    desc.OwnerLeafPolicy,
	}

	return result, nil
}

// encodeOutPoint converts wire.OutPoint to proto form.
func encodeOutPoint(op wire.OutPoint) *OOROutPoint {
	return &OOROutPoint{
		Txid: op.Hash.CloneBytes(),
		Vout: op.Index,
	}
}

// decodeOutPoint converts proto outpoint to wire.OutPoint.
func decodeOutPoint(op *OOROutPoint) (wire.OutPoint, error) {
	if op == nil {
		return wire.OutPoint{}, fmt.Errorf("outpoint is required")
	}

	if len(op.Txid) != chainhash.HashSize {
		return wire.OutPoint{}, fmt.Errorf("invalid outpoint txid "+
			"length: got %d want %d", len(op.Txid),
			chainhash.HashSize)
	}

	var hash chainhash.Hash
	copy(hash[:], op.Txid)

	return wire.OutPoint{
		Hash:  hash,
		Index: op.Vout,
	}, nil
}

// decodeSessionID converts a 32-byte session id into chainhash.Hash.
func decodeSessionID(raw []byte) (chainhash.Hash, error) {
	if len(raw) != chainhash.HashSize {
		return chainhash.Hash{}, fmt.Errorf("invalid session id "+
			"length: got %d want %d", len(raw), chainhash.HashSize)
	}

	var hash chainhash.Hash
	copy(hash[:], raw)

	return hash, nil
}

// encodePSBTSlice serializes a slice of PSBT packets.
func encodePSBTSlice(packets []*psbt.Packet) ([][]byte, error) {
	out := make([][]byte, 0, len(packets))
	for i := range packets {
		raw, err := psbtutil.Serialize(packets[i])
		if err != nil {
			return nil, err
		}

		out = append(out, raw)
	}

	return out, nil
}

// decodePSBTSlice parses a slice of serialized PSBT packets.
func decodePSBTSlice(rawPSBTs [][]byte) ([]*psbt.Packet, error) {
	out := make([]*psbt.Packet, 0, len(rawPSBTs))
	for i := range rawPSBTs {
		packet, err := psbtutil.Parse(rawPSBTs[i])
		if err != nil {
			return nil, err
		}

		out = append(out, packet)
	}

	return out, nil
}
