package oor

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
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

	// FailReason is the terminal failure reason, when Phase is Failed.
	FailReason string

	// RecipientPkScript is the persisted hint used while the incoming
	// package still needs resolution.
	RecipientPkScript []byte

	// RecipientEventID is the per-script cursor hint paired with
	// RecipientPkScript during resolve-pending restart.
	RecipientEventID uint64
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
// durable snapshot.
func NewReceiveSessionFromSnapshot(ctx context.Context,
	snapshot *IncomingSnapshot) (*ReceiveSession, error) {

	if snapshot == nil {
		return nil, fmt.Errorf("snapshot must be provided")
	}

	state, err := IncomingStateFromSnapshot(snapshot)
	if err != nil {
		return nil, err
	}

	return newReceiveSessionWithState(
		ctx, snapshot.SessionID, state,
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
