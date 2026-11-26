package oor

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/wavelength/baselib/tlvutil"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
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
	incomingSnapshotTaprootAssetTransferType  tlv.Type = 25
	incomingSnapshotCompletedEventIDsType     tlv.Type = 27
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

	// TaprootAssetTransfer carries the sealed Taproot Asset packages for an
	// incoming asset OOR transfer while materialization is pending.
	TaprootAssetTransfer *oortx.TaprootAssetTransfer

	// FailReason is the terminal failure reason, when Phase is Failed.
	FailReason string

	// RecipientPkScript is the persisted hint used while the incoming
	// package still needs resolution.
	RecipientPkScript []byte

	// RecipientEventID is the per-script cursor hint paired with
	// RecipientPkScript during resolve-pending restart and carried through
	// ReceiveNotified/ReceiveAwaitingAck until ReceiveCompleted records it.
	RecipientEventID uint64

	// CompletedEventIDs records recipient event IDs already processed by
	// this session across sequential receive passes.
	CompletedEventIDs []uint64

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
		Version:   1,
		SessionID: sessionID,
	}

	switch s := state.(type) {
	case *ReceiveResolving:
		snap.Phase = IncomingPhaseResolvePending
		snap.RecipientPkScript = append(
			[]byte(nil), s.RecipientPkScript...,
		)
		snap.RecipientEventID = s.RecipientEventID
		snap.CompletedEventIDs = append(
			[]uint64(nil), s.CompletedEventIDs...,
		)
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
		snap.TaprootAssetTransfer = s.TaprootAssetTransfer.Clone()
		snap.RecipientEventID = s.RecipientEventID
		snap.CompletedEventIDs = append(
			[]uint64(nil), s.CompletedEventIDs...,
		)
		snap.MetadataAttempts = s.MetadataAttempts

	case *ReceiveAwaitingAck:
		snap.Phase = IncomingPhaseAckPending
		snap.RecipientEventID = s.RecipientEventID
		snap.CompletedEventIDs = append(
			[]uint64(nil), s.CompletedEventIDs...,
		)

	case *ReceiveCompleted:
		snap.Phase = IncomingPhaseCompleted
		snap.CompletedEventIDs = append(
			[]uint64(nil), s.CompletedEventIDs...,
		)

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
			CompletedEventIDs: append(
				[]uint64(nil), snapshot.CompletedEventIDs...,
			),
			ResolveAttempts: snapshot.ResolveAttempts,
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
			TaprootAssetTransfer: snapshot.TaprootAssetTransfer.
				Clone(),
			RecipientEventID: snapshot.RecipientEventID,
			CompletedEventIDs: append(
				[]uint64(nil), snapshot.CompletedEventIDs...,
			),
			MetadataAttempts: snapshot.MetadataAttempts,
		}, nil

	case IncomingPhaseAckPending:
		return &ReceiveAwaitingAck{
			SessionID:        snapshot.SessionID,
			RecipientEventID: snapshot.RecipientEventID,
			CompletedEventIDs: append(
				[]uint64(nil), snapshot.CompletedEventIDs...,
			),
		}, nil

	case IncomingPhaseCompleted:
		return &ReceiveCompleted{
			CompletedEventIDs: append(
				[]uint64(nil), snapshot.CompletedEventIDs...,
			),
		}, nil

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

	var assetTransferRaw []byte
	if snapshot.TaprootAssetTransfer != nil {
		assetTransferRaw, err = snapshot.TaprootAssetTransfer.
			MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("encode incoming snapshot "+
				"taproot asset transfer: %w", err)
		}
	}

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
	}
	if len(assetTransferRaw) > 0 {
		records = append(
			records, tlv.MakePrimitiveRecord(
				incomingSnapshotTaprootAssetTransferType,
				&assetTransferRaw,
			),
		)
	}
	if len(snapshot.CompletedEventIDs) > 0 {
		completedIDsRaw := encodeUint64List(snapshot.CompletedEventIDs)
		records = append(
			records, tlv.MakePrimitiveRecord(
				incomingSnapshotCompletedEventIDsType,
				&completedIDsRaw,
			),
		)
	}

	return tlvutil.EncodeRecordsToBytes(records...)
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
		assetTransferRaw  []byte
		completedIDsRaw   []byte
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
			incomingSnapshotTaprootAssetTransferType,
			&assetTransferRaw,
		),
		tlv.MakePrimitiveRecord(
			incomingSnapshotCompletedEventIDsType, &completedIDsRaw,
		),
	}

	if _, err := decodeBoundedRecords(
		raw, records...,
	); err != nil {
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

	var decodedAssetTransfer *oortx.TaprootAssetTransfer
	if len(assetTransferRaw) > 0 {
		var transfer oortx.TaprootAssetTransfer
		if err := transfer.UnmarshalBinary(
			assetTransferRaw,
		); err != nil {
			return nil, fmt.Errorf("decode incoming snapshot "+
				"taproot asset transfer: %w", err)
		}
		if err := transfer.Validate(len(checkpoints)); err != nil {
			return nil, fmt.Errorf("validate incoming snapshot "+
				"taproot asset transfer: %w", err)
		}
		decodedAssetTransfer = &transfer
	}

	completedEventIDs, err := decodeUint64List(
		completedIDsRaw, maxCompletedIncomingEventIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("decode incoming snapshot completed "+
			"event ids: %w", err)
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
		Recipients:           CloneArkRecipients(decodedRecipients),
		TaprootAssetTransfer: decodedAssetTransfer,
		FailReason:           string(failReasonRaw),
		RecipientPkScript: append(
			[]byte(nil), recipientPkScript...,
		),
		RecipientEventID:  recipientEventID,
		CompletedEventIDs: completedEventIDs,
		MetadataAttempts:  uint32(metadataAttempts),
		ResolveAttempts:   uint32(resolveAttempts),
	}, nil
}

// encodeUint64List encodes a slice of uint64 values as big-endian 8-byte words.
func encodeUint64List(ids []uint64) []byte {
	out := make([]byte, len(ids)*8)
	for i, id := range ids {
		binary.BigEndian.PutUint64(out[i*8:(i+1)*8], id)
	}

	return out
}

// decodeUint64List decodes big-endian 8-byte words into a bounded uint64 slice.
func decodeUint64List(raw []byte, maxCount int) ([]uint64, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw)%8 != 0 {
		return nil, fmt.Errorf("byte length %d is not a multiple of 8",
			len(raw))
	}
	count := len(raw) / 8
	if maxCount > 0 && count > maxCount {
		return nil, fmt.Errorf("count %d exceeds limit %d", count,
			maxCount)
	}
	out := make([]uint64, count)
	for i := range count {
		out[i] = binary.BigEndian.Uint64(raw[i*8 : (i+1)*8])
	}

	return out, nil
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
