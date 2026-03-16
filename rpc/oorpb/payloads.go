package oorpb

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
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
	Outpoint  wire.OutPoint
	OwnerKey  *btcec.PublicKey
	ExitDelay uint32

	// SpendWitnessScript is set for custom spend paths (e.g., vHTLC).
	SpendWitnessScript []byte

	// SpendControlBlock is the BIP-341 control block for the custom
	// leaf.
	SpendControlBlock []byte
}

// NewSubmitPackageRequest builds a typed proto request for SubmitPackage.
func NewSubmitPackageRequest(ark *psbt.Packet, checkpoints []*psbt.Packet,
	descs []SigningDescriptor) (*SubmitPackageRequest, error) {

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

	return &SubmitPackageRequest{
		ArkPsbt:            arkRaw,
		CheckpointPsbts:    checkpointRaw,
		SigningDescriptors: protoDescs,
	}, nil
}

// ParseSubmitPackageRequest decodes a SubmitPackageRequest into domain types.
func ParseSubmitPackageRequest(req *SubmitPackageRequest) (*psbt.Packet,
	[]*psbt.Packet, []SigningDescriptor, error) {

	if req == nil {
		return nil, nil, nil, fmt.Errorf("submit request is required")
	}

	ark, err := psbtutil.Parse(req.ArkPsbt)
	if err != nil {
		return nil, nil, nil, err
	}

	checkpoints, err := decodePSBTSlice(req.CheckpointPsbts)
	if err != nil {
		return nil, nil, nil, err
	}

	descs := make([]SigningDescriptor, 0, len(req.SigningDescriptors))
	for i := range req.SigningDescriptors {
		desc, err := decodeSigningDescriptor(
			req.SigningDescriptors[i], i,
		)
		if err != nil {
			return nil, nil, nil, err
		}

		descs = append(descs, desc)
	}

	return ark, checkpoints, descs, nil
}

// NewSubmitPackageResponse builds a typed proto response for SubmitPackage.
func NewSubmitPackageResponse(sessionID chainhash.Hash,
	coSignedCheckpoints []*psbt.Packet) (*SubmitPackageResponse, error) {

	checkpointRaw, err := encodePSBTSlice(coSignedCheckpoints)
	if err != nil {
		return nil, err
	}

	return &SubmitPackageResponse{
		SessionId:               sessionID.CloneBytes(),
		CoSignedCheckpointPsbts: checkpointRaw,
	}, nil
}

// ParseSubmitPackageResponse decodes a SubmitPackageResponse.
func ParseSubmitPackageResponse(resp *SubmitPackageResponse) (chainhash.Hash,
	[]*psbt.Packet, error) {

	if resp == nil {
		return chainhash.Hash{}, nil,
			fmt.Errorf("submit response is required")
	}

	sessionID, err := decodeSessionID(resp.SessionId)
	if err != nil {
		return chainhash.Hash{}, nil, err
	}

	checkpoints, err := decodePSBTSlice(resp.CoSignedCheckpointPsbts)
	if err != nil {
		return chainhash.Hash{}, nil, err
	}

	return sessionID, checkpoints, nil
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
func ParseFinalizePackageResponse(
	resp *FinalizePackageResponse,
) (chainhash.Hash, error) {

	if resp == nil {
		return chainhash.Hash{},
			fmt.Errorf("finalize response is required")
	}

	return decodeSessionID(resp.SessionId)
}

// encodeSigningDescriptor converts one descriptor to proto form.
func encodeSigningDescriptor(desc SigningDescriptor,
	index int) (*OORSigningDescriptor, error) {

	if desc.OwnerKey == nil {
		return nil, fmt.Errorf(
			"signing descriptor %d missing owner key", index,
		)
	}

	return &OORSigningDescriptor{
		Outpoint:  encodeOutPoint(desc.Outpoint),
		OwnerKey:  desc.OwnerKey.SerializeCompressed(),
		ExitDelay: desc.ExitDelay,
	}, nil
}

// decodeSigningDescriptor converts one proto descriptor to domain form.
func decodeSigningDescriptor(desc *OORSigningDescriptor,
	index int) (SigningDescriptor, error) {

	if desc == nil {
		return SigningDescriptor{}, fmt.Errorf(
			"signing descriptor %d is nil", index,
		)
	}

	outpoint, err := decodeOutPoint(desc.Outpoint)
	if err != nil {
		return SigningDescriptor{}, err
	}

	ownerKey, err := btcec.ParsePubKey(desc.OwnerKey)
	if err != nil {
		return SigningDescriptor{}, err
	}

	return SigningDescriptor{
		Outpoint:  outpoint,
		OwnerKey:  ownerKey,
		ExitDelay: desc.ExitDelay,
	}, nil
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
		return wire.OutPoint{}, fmt.Errorf(
			"invalid outpoint txid length: got %d want %d",
			len(op.Txid), chainhash.HashSize,
		)
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
		return chainhash.Hash{}, fmt.Errorf(
			"invalid session id length: got %d want %d",
			len(raw), chainhash.HashSize,
		)
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
