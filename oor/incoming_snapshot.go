package oor

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
)

// IncomingPhase identifies the coarse stage of an incoming client transfer.
type IncomingPhase string

const (
	// incomingSnapshotVersion is the current encoding version for durable
	// incoming snapshots.
	incomingSnapshotVersion uint8 = 1

	// IncomingPhaseNotified indicates the transfer was validated and surfaced
	// to the application layer, but local materialization is still pending.
	IncomingPhaseNotified IncomingPhase = "notified"

	// IncomingPhaseAwaitingAck indicates local materialization succeeded and
	// the client still needs to ack the transfer to the server.
	IncomingPhaseAwaitingAck IncomingPhase = "awaiting_ack"

	// IncomingPhaseCompleted indicates incoming handling completed.
	IncomingPhaseCompleted IncomingPhase = "completed"
)

// IncomingSnapshot is a durable snapshot of an incoming OOR transfer session.
type IncomingSnapshot struct {
	// Version is the snapshot version.
	Version uint8

	// SessionID is the stable incoming session identifier.
	SessionID SessionID

	// Phase is the coarse incoming transfer phase.
	Phase IncomingPhase

	// ArkPSBT is the canonical Ark tx PSBT (serialized).
	ArkPSBT []byte

	// FinalCheckpointPSBTs are finalized checkpoint PSBTs associated with the
	// incoming transfer (serialized).
	FinalCheckpointPSBTs [][]byte
}

// NewIncomingSnapshot exports an incoming transfer FSM state into a snapshot.
func NewIncomingSnapshot(sessionID SessionID, state ReceiveState) (
	*IncomingSnapshot, error) {

	if sessionID == (SessionID{}) {
		return nil, fmt.Errorf("session id must be provided")
	}

	if state == nil {
		return nil, fmt.Errorf("state must be provided")
	}

	snap := &IncomingSnapshot{
		Version:   incomingSnapshotVersion,
		SessionID: sessionID,
	}

	switch s := state.(type) {
	case *ReceiveIdle:
		return nil, fmt.Errorf("cannot snapshot idle incoming state")

	case *ReceiveNotified:
		snap.Phase = IncomingPhaseNotified

		ark, err := psbtutil.Serialize(s.ArkPSBT)
		if err != nil {
			return nil, err
		}
		snap.ArkPSBT = ark

		finalized, err := serializeIncomingPSBTSlice(
			s.FinalCheckpointPSBTs,
		)
		if err != nil {
			return nil, err
		}
		snap.FinalCheckpointPSBTs = finalized

	case *ReceiveAwaitingAck:
		snap.Phase = IncomingPhaseAwaitingAck

	case *ReceiveCompleted:
		snap.Phase = IncomingPhaseCompleted

	default:
		return nil, fmt.Errorf("unsupported incoming state type: %T",
			state)
	}

	return snap, nil
}

// NewReceiveSessionFromSnapshot restores a receive session from snapshot.
func NewReceiveSessionFromSnapshot(ctx context.Context,
	snapshot *IncomingSnapshot) (*ReceiveSession, error) {

	if snapshot == nil {
		return nil, fmt.Errorf("snapshot must be provided")
	}

	state, err := IncomingStateFromSnapshot(snapshot)
	if err != nil {
		return nil, err
	}

	env := &Environment{SessionID: snapshot.SessionID}
	fsmCfg := StateMachineCfg{
		Logger: log.WithPrefix(snapshot.SessionID.LogPrefix()),
		ErrorReporter: newContextErrorReporter(
			ctx, snapshot.SessionID.LogPrefix(),
		),
		InitialState: state,
		Env:          env,
	}

	sm := protofsm.NewStateMachine(fsmCfg)
	sm.Start(ctx)

	return &ReceiveSession{
		ID:  snapshot.SessionID,
		FSM: &sm,
	}, nil
}

// IncomingStateFromSnapshot converts a snapshot into an incoming state.
func IncomingStateFromSnapshot(snapshot *IncomingSnapshot) (ReceiveState, error) {
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
	case IncomingPhaseNotified:
		ark, err := parseIncomingArkPSBT(snapshot.ArkPSBT)
		if err != nil {
			return nil, err
		}

		finalized, err := parseIncomingPSBTSlice(
			snapshot.FinalCheckpointPSBTs,
		)
		if err != nil {
			return nil, err
		}

		return &ReceiveNotified{
			SessionID:            snapshot.SessionID,
			ArkPSBT:              ark,
			FinalCheckpointPSBTs: finalized,
		}, nil

	case IncomingPhaseAwaitingAck:
		return &ReceiveAwaitingAck{
			SessionID: snapshot.SessionID,
		}, nil

	case IncomingPhaseCompleted:
		return &ReceiveCompleted{}, nil

	default:
		return nil, fmt.Errorf("unknown incoming snapshot phase: %s",
			snapshot.Phase)
	}
}

// OutboxForReceiveState returns the outbox implied by the current incoming
// session state.
func OutboxForReceiveState(state ReceiveState) ([]OutboxEvent, error) {
	if state == nil {
		return nil, fmt.Errorf("state must be provided")
	}

	switch s := state.(type) {
	case *ReceiveIdle:
		return nil, nil

	case *ReceiveNotified:
		recipients, err := ExtractArkRecipients(s.ArkPSBT)
		if err != nil {
			return nil, err
		}

		return []OutboxEvent{
			&MaterializeIncomingVTXOsRequest{
				SessionID:            s.SessionID,
				ArkPSBT:              s.ArkPSBT,
				FinalCheckpointPSBTs: s.FinalCheckpointPSBTs,
				Recipients:           recipients,
			},
		}, nil

	case *ReceiveAwaitingAck:
		return []OutboxEvent{
			&SendIncomingAckRequest{
				SessionID: s.SessionID,
			},
		}, nil

	case *ReceiveCompleted:
		return nil, nil

	default:
		return nil, fmt.Errorf("unsupported incoming state type: %T",
			state)
	}
}

// parseIncomingArkPSBT parses a serialized Ark PSBT.
func parseIncomingArkPSBT(raw []byte) (*psbt.Packet, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("ark psbt must be provided")
	}

	return psbtutil.Parse(raw)
}

// parseIncomingPSBTSlice parses serialized PSBT packets.
func parseIncomingPSBTSlice(raw [][]byte) ([]*psbt.Packet, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	out := make([]*psbt.Packet, 0, len(raw))
	for _, pktRaw := range raw {
		pkt, err := psbtutil.Parse(pktRaw)
		if err != nil {
			return nil, err
		}

		out = append(out, pkt)
	}

	return out, nil
}

// serializeIncomingPSBTSlice serializes a list of PSBT packets.
func serializeIncomingPSBTSlice(packets []*psbt.Packet) ([][]byte, error) {
	if len(packets) == 0 {
		return nil, nil
	}

	out := make([][]byte, 0, len(packets))
	for _, pkt := range packets {
		if pkt == nil {
			return nil, fmt.Errorf("psbt must be provided")
		}

		raw, err := psbtutil.Serialize(pkt)
		if err != nil {
			return nil, err
		}

		out = append(out, raw)
	}

	return out, nil
}
