package oor

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	incomingSnapshotVersionRecordType         tlv.Type = 1
	incomingSnapshotSessionIDRecordType       tlv.Type = 3
	incomingSnapshotPhaseRecordType           tlv.Type = 5
	incomingSnapshotArkPSBTRecordType         tlv.Type = 7
	incomingSnapshotCheckpointPSBTsRecordType tlv.Type = 9
	incomingSnapshotFailReasonRecordType      tlv.Type = 11
	incomingSnapshotRecipientPkScriptType     tlv.Type = 13
	incomingSnapshotRecipientEventIDType      tlv.Type = 15
	incomingSnapshotAncestorPackagesType      tlv.Type = 17
	incomingSnapshotMetadataAttemptsType      tlv.Type = 19
	incomingSnapshotResolveAttemptsType       tlv.Type = 21
	incomingSnapshotRecipientsType            tlv.Type = 23
	incomingSnapshotAssetTransferType         tlv.Type = 25
)

// IncomingPhase identifies the coarse stage of an incoming OOR receive
// session.
type IncomingPhase string

const (
	// IncomingPhaseResolvePending indicates the client durably recorded
	// an incoming-hint notification and still needs to resolve the full
	// Ark/checkpoint package outside the actor transaction.
	IncomingPhaseResolvePending IncomingPhase = "resolve_pending"

	// IncomingPhaseMaterializePending indicates the incoming transfer has
	// been validated and notified locally, and the authoritative metadata
	// lookup/materialization work still needs to run.
	IncomingPhaseMaterializePending IncomingPhase = "materialize_pending"

	// IncomingPhaseAckPending indicates the incoming VTXOs were
	// durably materialized and the client still needs to enqueue the
	// best-effort ack to the server.
	IncomingPhaseAckPending IncomingPhase = "ack_pending"

	// IncomingPhaseCompleted indicates the incoming transfer finished.
	IncomingPhaseCompleted IncomingPhase = "completed"

	// IncomingPhaseFailed indicates the incoming transfer entered a
	// terminal failure state.
	IncomingPhaseFailed IncomingPhase = "failed"
)

// IncomingSnapshot is the durable checkpoint shape for incoming OOR receive
// sessions.
type IncomingSnapshot struct {
	// Version is the snapshot version.
	Version uint8

	// SessionID is the stable Ark txid for the incoming transfer.
	SessionID SessionID

	// Phase is the coarse incoming receive phase.
	Phase IncomingPhase

	// ArkPSBT is the canonical Ark transfer PSBT.
	ArkPSBT []byte

	// CheckpointPSBTs are the finalized checkpoint PSBTs
	// associated with the incoming transfer when the phase
	// needs them.
	CheckpointPSBTs [][]byte

	// AncestorPackages are finalized OOR package artifacts needed to
	// unroll chained incoming transfers.
	AncestorPackages []PackageArtifact

	// Recipients are the validated Ark outputs plus optional policy
	// metadata needed to materialize custom incoming VTXOs.
	Recipients []ArkRecipientOutput

	// TaprootAssetTransfer is the encoded sealed package container for the
	// incoming session.
	TaprootAssetTransfer []byte

	// FailReason is the terminal failure reason, when Phase is Failed.
	FailReason string

	// RecipientPkScript is the persisted hint used while the incoming
	// package still needs resolution.
	RecipientPkScript []byte

	// RecipientEventID is the per-script cursor hint paired with
	// RecipientPkScript during resolve-pending restart.
	RecipientEventID uint64

	// MetadataAttempts is the persisted retry count for the authoritative
	// metadata resolution, used to bound backoff and the terminal give-up
	// across restarts. Only meaningful in the materialize-pending phase.
	MetadataAttempts uint32

	// ResolveAttempts is the persisted retry count for the phase-1 hint
	// resolution, used to bound the give-up timer across restarts. Only
	// meaningful in the resolve-pending phase.
	ResolveAttempts uint32
}

// NewIncomingSnapshot exports an incoming receive session state into a
// checkpoint snapshot.
func NewIncomingSnapshot(sessionID SessionID,
	state SessionState) (*IncomingSnapshot, error) {

	if sessionID == (SessionID{}) {
		return nil, fmt.Errorf("session id must be provided")
	}

	if state == nil {
		return nil, fmt.Errorf("state must be provided")
	}

	snap := &IncomingSnapshot{
		// Version 3 adds SDK-neutral asset identity and amount within
		// the incoming recipient record. The appended TLV fields are
		// optional.
		Version:   3,
		SessionID: sessionID,
	}

	switch s := state.(type) {
	case *ReceiveResolving:
		snap.Phase = IncomingPhaseResolvePending
		snap.RecipientPkScript = append(
			[]byte(nil), s.RecipientPkScript...,
		)
		snap.RecipientEventID = s.RecipientEventID
		snap.ResolveAttempts = s.ResolveAttempts

	case *ReceiveNotified:
		ark, err := psbtutil.Serialize(s.ArkPSBT)
		if err != nil {
			return nil, err
		}

		checkpoints, err := serializePSBTSlice(
			s.FinalCheckpointPSBTs,
		)
		if err != nil {
			return nil, err
		}

		snap.Phase = IncomingPhaseMaterializePending
		snap.ArkPSBT = ark
		snap.CheckpointPSBTs = checkpoints
		snap.AncestorPackages = append(
			[]PackageArtifact(nil), s.AncestorPackages...,
		)
		snap.Recipients = CloneArkRecipients(s.Recipients)
		if s.TaprootAssetTransfer != nil {
			snap.TaprootAssetTransfer, err =
				s.TaprootAssetTransfer.MarshalBinary()
			if err != nil {
				return nil, err
			}
		}
		snap.MetadataAttempts = s.MetadataAttempts

	case *ReceiveAwaitingAck:
		snap.Phase = IncomingPhaseAckPending

	case *ReceiveCompleted:
		snap.Phase = IncomingPhaseCompleted

	case *Failed:
		snap.Phase = IncomingPhaseFailed
		snap.FailReason = s.Reason

	default:
		return nil, fmt.Errorf("unsupported incoming state type: %T",
			state)
	}

	return snap, nil
}

// NewReceiveSessionFromSnapshot restores an incoming receive session from a
// durable snapshot. envCfg injects the deterministic clock and retry budget
// into the restored FSM Environment.
func NewReceiveSessionFromSnapshot(ctx context.Context,
	snapshot *IncomingSnapshot, envCfg EnvConfig) (*ReceiveSession, error) {

	if snapshot == nil {
		return nil, fmt.Errorf("snapshot must be provided")
	}

	state, err := IncomingStateFromSnapshot(snapshot)
	if err != nil {
		return nil, err
	}

	return newReceiveSessionWithState(
		ctx, snapshot.SessionID, state, envCfg,
	)
}

// IncomingStateFromSnapshot converts an incoming receive snapshot into the
// corresponding concrete state.
func IncomingStateFromSnapshot(snapshot *IncomingSnapshot) (SessionState,
	error) {

	if snapshot == nil {
		return nil, fmt.Errorf("snapshot must be provided")
	}

	if snapshot.SessionID == (SessionID{}) {
		return nil, fmt.Errorf("session id must be provided")
	}

	if snapshot.Version == 0 {
		return nil, fmt.Errorf("snapshot version must be provided")
	}

	switch snapshot.Phase {
	case IncomingPhaseResolvePending:
		return &ReceiveResolving{
			SessionID: snapshot.SessionID,
			RecipientPkScript: append(
				[]byte(nil), snapshot.RecipientPkScript...,
			),
			RecipientEventID: snapshot.RecipientEventID,
			ResolveAttempts:  snapshot.ResolveAttempts,
		}, nil

	case IncomingPhaseMaterializePending:
		ark, checkpoints, err := parseIncomingPSBTs(
			snapshot.ArkPSBT, snapshot.CheckpointPSBTs,
		)
		if err != nil {
			return nil, err
		}

		err = requireSessionIDMatchesArk(snapshot.SessionID, ark)
		if err != nil {
			return nil, err
		}

		assetTransfer, err := decodeTaprootAssetTransfer(
			snapshot.TaprootAssetTransfer, len(checkpoints),
		)
		if err != nil {
			return nil, err
		}

		return &ReceiveNotified{
			SessionID:            snapshot.SessionID,
			ArkPSBT:              ark,
			FinalCheckpointPSBTs: checkpoints,
			AncestorPackages: append(
				[]PackageArtifact(nil),
				snapshot.AncestorPackages...,
			),
			Recipients: CloneArkRecipients(
				snapshot.Recipients,
			),
			MetadataAttempts:     snapshot.MetadataAttempts,
			TaprootAssetTransfer: assetTransfer,
		}, nil

	case IncomingPhaseAckPending:
		return &ReceiveAwaitingAck{
			SessionID: snapshot.SessionID,
		}, nil

	case IncomingPhaseCompleted:
		return &ReceiveCompleted{}, nil

	case IncomingPhaseFailed:
		return &Failed{Reason: snapshot.FailReason}, nil

	default:
		return nil, fmt.Errorf("unknown incoming phase: %s",
			snapshot.Phase)
	}
}

// encodeIncomingSnapshot serializes an incoming snapshot into TLV bytes.
func encodeIncomingSnapshot(snapshot *IncomingSnapshot) ([]byte, error) {
	if snapshot == nil {
		return nil, fmt.Errorf("snapshot must be provided")
	}

	sessionBytes := sessionIDBytes(snapshot.SessionID)
	phaseBytes := []byte(snapshot.Phase)
	arkPSBT := snapshot.ArkPSBT
	checkpoints, err := encodeLengthPrefixedBlobList(
		snapshot.CheckpointPSBTs,
	)
	if err != nil {
		return nil, err
	}

	failReason := []byte(snapshot.FailReason)
	recipientPkScript := snapshot.RecipientPkScript
	recipientEventID := snapshot.RecipientEventID
	ancestorPackages, err := encodePackageArtifacts(
		snapshot.AncestorPackages,
	)
	if err != nil {
		return nil, err
	}

	recipients, err := encodeIncomingRecipients(snapshot.Recipients)
	if err != nil {
		return nil, err
	}
	assetTransfer := snapshot.TaprootAssetTransfer

	metadataAttempts := uint64(snapshot.MetadataAttempts)
	resolveAttempts := uint64(snapshot.ResolveAttempts)
	version := uint64(snapshot.Version)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			incomingSnapshotVersionRecordType, &version,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotSessionIDRecordType, &sessionBytes,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotPhaseRecordType, &phaseBytes,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotArkPSBTRecordType, &arkPSBT,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotCheckpointPSBTsRecordType, &checkpoints,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotFailReasonRecordType, &failReason,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotRecipientPkScriptType,
			&recipientPkScript,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotRecipientEventIDType, &recipientEventID,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotAncestorPackagesType, &ancestorPackages,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotMetadataAttemptsType, &metadataAttempts,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotResolveAttemptsType, &resolveAttempts,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotRecipientsType, &recipients,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotAssetTransferType, &assetTransfer,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// decodeIncomingSnapshotWithLimits decodes one incoming snapshot and applies
// receive limits to nested checkpoint and ancestor-package lists.
func decodeIncomingSnapshotWithLimits(raw []byte,
	limits ReceiveLimits) (*IncomingSnapshot, error) {

	var (
		version           uint64
		sessionBytes      []byte
		phaseBytes        []byte
		arkPSBT           []byte
		checkpointsRaw    []byte
		failReasonRaw     []byte
		recipientPkScript []byte
		recipientEventID  uint64
		ancestorPackages  []byte
		metadataAttempts  uint64
		resolveAttempts   uint64
		recipientsRaw     []byte
		assetTransfer     []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			incomingSnapshotVersionRecordType, &version,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotSessionIDRecordType, &sessionBytes,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotPhaseRecordType, &phaseBytes,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotArkPSBTRecordType, &arkPSBT,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotCheckpointPSBTsRecordType,
			&checkpointsRaw,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotFailReasonRecordType, &failReasonRaw,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotRecipientPkScriptType,
			&recipientPkScript,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotRecipientEventIDType, &recipientEventID,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotAncestorPackagesType, &ancestorPackages,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotMetadataAttemptsType, &metadataAttempts,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotResolveAttemptsType, &resolveAttempts,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotRecipientsType, &recipientsRaw,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotAssetTransferType, &assetTransfer,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return nil, err
	}

	sessionID, err := parseSessionID(sessionBytes)
	if err != nil {
		return nil, err
	}

	checkpoints, err := decodeLengthPrefixedBlobListWithLimits(
		checkpointsRaw, limits,
	)
	if err != nil {
		return nil, err
	}
	if len(checkpoints) == 0 {
		checkpoints = nil
	}

	if len(arkPSBT) == 0 {
		arkPSBT = nil
	}

	decodedPackages, err := decodePackageArtifactsWithLimits(
		ancestorPackages, limits,
	)
	if err != nil {
		return nil, err
	}

	decodedRecipients, err := decodeIncomingRecipientsWithLimits(
		recipientsRaw, limits,
	)
	if err != nil {
		return nil, err
	}

	decodedVersion, err := decodeUint64ToUint8(
		version, "incoming snapshot version",
	)
	if err != nil {
		return nil, err
	}

	return &IncomingSnapshot{
		Version:         decodedVersion,
		SessionID:       sessionID,
		Phase:           IncomingPhase(phaseBytes),
		ArkPSBT:         arkPSBT,
		CheckpointPSBTs: checkpoints,
		AncestorPackages: append(
			[]PackageArtifact(nil), decodedPackages...,
		),
		Recipients: CloneArkRecipients(decodedRecipients),
		TaprootAssetTransfer: append(
			[]byte(nil), assetTransfer...,
		),
		FailReason: string(failReasonRaw),
		RecipientPkScript: append(
			[]byte(nil), recipientPkScript...,
		),
		RecipientEventID: recipientEventID,
		MetadataAttempts: uint32(metadataAttempts),
		ResolveAttempts:  uint32(resolveAttempts),
	}, nil
}

// parseIncomingPSBTs parses the Ark and checkpoint PSBTs stored in an incoming
// snapshot.
func parseIncomingPSBTs(arkRaw []byte, checkpointRaws [][]byte) (*psbt.Packet,
	[]*psbt.Packet, error) {

	ark, err := psbtutil.Parse(arkRaw)
	if err != nil {
		return nil, nil, err
	}

	checkpoints, err := parsePSBTSlice(checkpointRaws)
	if err != nil {
		return nil, nil, err
	}

	return ark, checkpoints, nil
}
