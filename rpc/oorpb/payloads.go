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

	// TaprootAssetRoot is the optional 32-byte Taproot Asset commitment
	// root anchored in the spent input VTXO.
	TaprootAssetRoot *chainhash.Hash

	// TaprootAssetRef is the canonical opaque SDK-level asset identity.
	TaprootAssetRef string

	// TaprootAssetAmount is the number of asset units carried by the spent
	// input VTXO.
	TaprootAssetAmount uint64
}

// NewSubmitPackageRequest builds a typed proto request for SubmitPackage.
func NewSubmitPackageRequest(ark *psbt.Packet, checkpoints []*psbt.Packet,
	descs []SigningDescriptor,
	recipients []oortx.RecipientOutput) (*SubmitPackageRequest, error) {

	return NewSubmitPackageRequestWithAssets(
		ark, checkpoints, descs, recipients, nil,
	)
}

// NewSubmitPackageRequestWithAssets builds a typed proto request for
// SubmitPackage with an optional sealed Taproot Asset transfer container.
func NewSubmitPackageRequestWithAssets(ark *psbt.Packet,
	checkpoints []*psbt.Packet, descs []SigningDescriptor,
	recipients []oortx.RecipientOutput,
	assetTransfer *oortx.TaprootAssetTransfer) (*SubmitPackageRequest,
	error) {

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
		protoRecipient, err := encodeRecipientOutput(recipients[i], i)
		if err != nil {
			return nil, err
		}

		protoRecipients = append(protoRecipients, protoRecipient)
	}

	if err := validateSubmitPackageAssetConsistency(
		len(checkpoints), descs, recipients, assetTransfer,
	); err != nil {
		return nil, err
	}

	var assetTransferRaw []byte
	if assetTransfer != nil {
		assetTransferRaw, err = assetTransfer.MarshalBinary()
		if err != nil {
			return nil, err
		}
	}

	return &SubmitPackageRequest{
		ArkPsbt:              arkRaw,
		CheckpointPsbts:      checkpointRaw,
		SigningDescriptors:   protoDescs,
		RecipientOutputs:     protoRecipients,
		TaprootAssetTransfer: assetTransferRaw,
	}, nil
}

// ParseSubmitPackageRequest decodes a plain Bitcoin SubmitPackageRequest into
// domain types. It fails closed if the request carries a Taproot Asset
// transfer container or any asset-bearing descriptor/recipient so legacy
// non-asset callers never silently ignore asset metadata.
func ParseSubmitPackageRequest(req *SubmitPackageRequest) (*psbt.Packet,
	[]*psbt.Packet, []SigningDescriptor, []oortx.RecipientOutput, error) {

	ark, checkpoints, descs, recipients, assetTransfer, err :=
		ParseSubmitPackageRequestWithAssets(req)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if assetTransfer != nil {
		return nil, nil, nil, nil, fmt.Errorf("submit request " +
			"carries taproot asset transfer; use " +
			"ParseSubmitPackageRequestWithAssets")
	}

	return ark, checkpoints, descs, recipients, nil
}

// ParseSubmitPackageRequestWithAssets decodes a SubmitPackageRequest into
// domain types and validates any attached Taproot Asset transfer container and
// asset descriptor/recipient consistency against the checkpoint set.
func ParseSubmitPackageRequestWithAssets(req *SubmitPackageRequest) (
	*psbt.Packet, []*psbt.Packet, []SigningDescriptor,
	[]oortx.RecipientOutput, *oortx.TaprootAssetTransfer, error) {

	if req == nil {
		return nil, nil, nil, nil, nil,
			fmt.Errorf("submit request is required")
	}

	ark, err := psbtutil.Parse(req.ArkPsbt)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	checkpoints, err := decodePSBTSlice(req.CheckpointPsbts)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	descs := make([]SigningDescriptor, 0, len(req.SigningDescriptors))
	for i := range req.SigningDescriptors {
		desc, err := decodeSigningDescriptor(
			req.SigningDescriptors[i], i,
		)
		if err != nil {
			return nil, nil, nil, nil, nil, err
		}

		descs = append(descs, desc)
	}

	recipients := make(
		[]oortx.RecipientOutput, 0, len(req.RecipientOutputs),
	)
	for i := range req.RecipientOutputs {
		recipient, err := decodeRecipientOutput(
			req.RecipientOutputs[i], i,
		)
		if err != nil {
			return nil, nil, nil, nil, nil, err
		}

		recipients = append(recipients, recipient)
	}

	var assetTransfer *oortx.TaprootAssetTransfer
	if len(req.TaprootAssetTransfer) > 0 {
		decoded := &oortx.TaprootAssetTransfer{}
		if err := decoded.UnmarshalBinary(
			req.TaprootAssetTransfer,
		); err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("decode "+
				"taproot asset transfer: %w", err)
		}
		assetTransfer = decoded
	}

	if err := validateSubmitPackageAssetConsistency(
		len(checkpoints), descs, recipients, assetTransfer,
	); err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("validate taproot "+
			"asset transfer: %w", err)
	}

	return ark, checkpoints, descs, recipients, assetTransfer, nil
}

// validateSubmitPackageAssetConsistency enforces that Taproot Asset fields on
// signing descriptors and recipients appear if and only if a matching
// TaprootAssetTransfer container is present, and that each non-empty container
// checkpoint slot aligns 1:1 with an asset-bearing signing descriptor.
func validateSubmitPackageAssetConsistency(checkpointCount int,
	descs []SigningDescriptor, recipients []oortx.RecipientOutput,
	assetTransfer *oortx.TaprootAssetTransfer) error {

	assetDescs := 0
	for i := range descs {
		if descs[i].TaprootAssetRoot != nil {
			assetDescs++
		}
	}

	assetRecipients := 0
	for i := range recipients {
		if recipients[i].Asset != nil {
			assetRecipients++
		}
	}

	if assetTransfer == nil {
		if assetDescs > 0 {
			return fmt.Errorf("taproot asset signing descriptors " +
				"require a taproot asset transfer container")
		}
		if assetRecipients > 0 {
			return fmt.Errorf("taproot asset recipient outputs " +
				"require a taproot asset transfer container")
		}

		return nil
	}

	if err := assetTransfer.Validate(checkpointCount); err != nil {
		return err
	}
	if len(descs) != checkpointCount {
		return fmt.Errorf("signing descriptor count %d does not match "+
			"checkpoint count %d", len(descs), checkpointCount)
	}
	if assetDescs == 0 {
		return fmt.Errorf("taproot asset transfer requires at least " +
			"one asset-bearing signing descriptor")
	}
	if assetRecipients == 0 {
		return fmt.Errorf("taproot asset transfer requires at least " +
			"one asset-bearing recipient output")
	}

	for i := range descs {
		hasDescAsset := descs[i].TaprootAssetRoot != nil
		hasSlot := len(assetTransfer.CheckpointPackages[i]) > 0
		if hasDescAsset != hasSlot {
			return fmt.Errorf("signing descriptor %d asset "+
				"presence does not match checkpoint "+
				"package slot", i)
		}
	}

	return nil
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
// serverBestHeight is the operator's best block height at rejection time; it
// is a diagnostic hint for transient rejections and may be 0 when the operator
// does not populate it.
func NewSubmitPackageRejection(sessionID chainhash.Hash, code OORRejectCode,
	reason string, serverBestHeight uint32) *SubmitPackageResponse {

	return &SubmitPackageResponse{
		Result: &SubmitPackageResponse_Rejection{
			Rejection: &SubmitPackageRejection{
				Code:             code,
				Reason:           reason,
				SessionId:        sessionID.CloneBytes(),
				ServerBestHeight: serverBestHeight,
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

		// Keep the wire decoder compatible with responses that omit
		// co_signed_ark_psbt. The OOR session requires the completed
		// Ark PSBT before it proceeds to checkpoint signing.
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
			Code:             r.Rejection.Code,
			Reason:           r.Rejection.Reason,
			ServerBestHeight: r.Rejection.ServerBestHeight,
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

	// ServerBestHeight is the operator's best block height at rejection
	// time, or 0 when the operator did not populate it. It is a diagnostic
	// hint for transient rejections (OOR_REJECT_INPUT_NOT_SPENDABLE) — a
	// value below the input's confirmation height confirms the operator is
	// merely behind on chain sync — but the retry decision routes on the
	// typed code alone, not on this value.
	ServerBestHeight uint32
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

	assetRoot, err := encodeOptionalAssetFields(
		desc.TaprootAssetRoot, desc.TaprootAssetRef,
		desc.TaprootAssetAmount,
	)
	if err != nil {
		return nil, fmt.Errorf("signing descriptor %d: %w", index, err)
	}

	proto := &OORSigningDescriptor{
		Outpoint:           encodeOutPoint(desc.Outpoint),
		VtxoPolicyTemplate: desc.VTXOPolicyTemplate,
		SpendPath:          desc.SpendPath,
		OwnerLeafPolicy:    desc.OwnerLeafPolicy,
		TaprootAssetRoot:   assetRoot,
		TaprootAssetRef:    desc.TaprootAssetRef,
		TaprootAssetAmount: desc.TaprootAssetAmount,
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

	asset, err := decodeOptionalAssetFields(
		desc.TaprootAssetRoot, desc.TaprootAssetRef,
		desc.TaprootAssetAmount,
	)
	if err != nil {
		return SigningDescriptor{}, fmt.Errorf("signing descriptor "+
			"%d: %w", index, err)
	}

	result := SigningDescriptor{
		Outpoint:           outpoint,
		VTXOPolicyTemplate: desc.VtxoPolicyTemplate,
		SpendPath:          desc.SpendPath,
		OwnerLeafPolicy:    desc.OwnerLeafPolicy,
	}
	if asset != nil {
		root := asset.Root
		result.TaprootAssetRoot = &root
		result.TaprootAssetRef = asset.Ref
		result.TaprootAssetAmount = asset.Amount
	}

	return result, nil
}

// encodeRecipientOutput converts one domain recipient output to proto form.
func encodeRecipientOutput(recipient oortx.RecipientOutput,
	index int) (*OORRecipientOutput, error) {

	if err := recipient.Asset.Validate(); err != nil {
		return nil, fmt.Errorf("recipient output %d: %w", index, err)
	}

	protoOut := &OORRecipientOutput{
		PkScript:           recipient.PkScript,
		ValueSat:           int64(recipient.Value),
		VtxoPolicyTemplate: recipient.VTXOPolicyTemplate,
	}
	if recipient.Asset != nil {
		protoOut.TaprootAssetRoot = recipient.Asset.Root.CloneBytes()
		protoOut.TaprootAssetRef = recipient.Asset.Ref
		protoOut.TaprootAssetAmount = recipient.Asset.Amount
	}

	return protoOut, nil
}

// decodeRecipientOutput converts one proto recipient output to domain form.
func decodeRecipientOutput(recipient *OORRecipientOutput,
	index int) (oortx.RecipientOutput, error) {

	if recipient == nil {
		return oortx.RecipientOutput{}, fmt.Errorf("recipient output "+
			"%d is nil", index)
	}

	asset, err := decodeOptionalAssetFields(
		recipient.TaprootAssetRoot, recipient.TaprootAssetRef,
		recipient.TaprootAssetAmount,
	)
	if err != nil {
		return oortx.RecipientOutput{}, fmt.Errorf("recipient output "+
			"%d: %w", index, err)
	}

	return oortx.RecipientOutput{
		PkScript:           recipient.PkScript,
		Value:              btcutil.Amount(recipient.ValueSat),
		VTXOPolicyTemplate: recipient.VtxoPolicyTemplate,
		Asset:              asset,
	}, nil
}

// encodeOptionalAssetFields validates an optional (root, ref, amount) triple
// and returns the raw 32-byte commitment root when present.
func encodeOptionalAssetFields(root *chainhash.Hash, ref string,
	amount uint64) ([]byte, error) {

	if root == nil && ref == "" && amount == 0 {
		return nil, nil
	}
	if root == nil {
		return nil, fmt.Errorf("asset metadata requires a commitment " +
			"root")
	}

	asset := &oortx.RecipientAsset{
		Root:   *root,
		Ref:    ref,
		Amount: amount,
	}
	if err := asset.Validate(); err != nil {
		return nil, err
	}

	return root.CloneBytes(), nil
}

// decodeOptionalAssetFields decodes and validates an optional wire (root, ref,
// amount) triple, enforcing all-or-none presence and bounds.
func decodeOptionalAssetFields(rootRaw []byte, ref string,
	amount uint64) (*oortx.RecipientAsset, error) {

	if len(rootRaw) == 0 && ref == "" && amount == 0 {
		return nil, nil
	}
	if len(rootRaw) != chainhash.HashSize {
		return nil, fmt.Errorf("invalid taproot asset root length: "+
			"got %d want %d", len(rootRaw), chainhash.HashSize)
	}

	var root chainhash.Hash
	copy(root[:], rootRaw)

	asset := &oortx.RecipientAsset{
		Root:   root,
		Ref:    ref,
		Amount: amount,
	}
	if err := asset.Validate(); err != nil {
		return nil, err
	}

	return asset, nil
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
