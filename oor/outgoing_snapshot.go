package oor

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
)

// OutgoingPhase identifies the coarse stage of an outgoing client transfer.
//
// This is intentionally more stable than Go state type names to keep snapshot
// formats evolvable.
type OutgoingPhase string

const (
	// OutgoingPhaseArkSignRequested indicates the submit package has been
	// built and the client must attach Ark signatures before submit can be
	// sent.
	OutgoingPhaseArkSignRequested OutgoingPhase = "ark_sign_requested"

	// OutgoingPhaseSubmitSent indicates the client has built the submit
	// package and is waiting for the server to accept/co-sign it.
	OutgoingPhaseSubmitSent OutgoingPhase = "submit_sent"

	// OutgoingPhaseCoSigned indicates the server accepted/co-signed and the
	// client must attach its checkpoint signatures.
	OutgoingPhaseCoSigned OutgoingPhase = "cosigned"

	// OutgoingPhaseFinalizeSent indicates the client has sent finalized
	// checkpoints and is awaiting server acknowledgement.
	OutgoingPhaseFinalizeSent OutgoingPhase = "finalize_sent"

	// OutgoingPhaseLocalVTXOUpdate indicates the server accepted finalize
	// and the client is updating its local VTXO persistence state.
	OutgoingPhaseLocalVTXOUpdate OutgoingPhase = "local_vtxo_update"

	// OutgoingPhaseAwaitingArkConfirmation indicates the client is waiting
	// for the Ark tx to confirm on-chain.
	OutgoingPhaseAwaitingArkConfirmation OutgoingPhase = "ark_confirm"

	// OutgoingPhaseRetryBackoff indicates the client is waiting to retry a
	// previously failed outbox request.
	OutgoingPhaseRetryBackoff OutgoingPhase = "retry_backoff"

	// OutgoingPhaseCompleted indicates the transfer is fully complete.
	OutgoingPhaseCompleted OutgoingPhase = "completed"

	// OutgoingPhaseFailed indicates the transfer entered a terminal failure
	// state.
	OutgoingPhaseFailed OutgoingPhase = "failed"
)

// OutgoingSnapshot is a client-side snapshot of an outgoing OOR transfer.
//
// This structure is intended to support mobile/restart semantics. The fields
// are intentionally "artifact heavy" (PSBT bytes) so an app can resume without
// needing to reconstruct taproot metadata or re-derive ordering rules.
//
// NOTE: TransferInputs contains rich Go types (tapscript/key descriptors) and
// is not currently encoded for cross-process durability. The durable actor work
// in darepo-client will eventually provide a canonical encoding for these.
//
// TransferInputSnapshots is the portable encoding used for persistence.
type OutgoingSnapshot struct {
	// Version is the snapshot version.
	Version uint8

	// SessionID is the stable session identifier (Ark txid).
	SessionID SessionID

	// Phase is the coarse outgoing transfer phase.
	Phase OutgoingPhase

	// ArkPSBT is the canonical Ark PSBT (serialized).
	ArkPSBT []byte

	// CheckpointPSBTs are checkpoint PSBTs relevant for the current phase
	// (serialized).
	CheckpointPSBTs [][]byte

	// TransferInputs are kept for in-process use and compatibility with
	// earlier snapshots. New snapshots should prefer
	// TransferInputSnapshots.
	TransferInputs []TransferInput

	// TransferInputSnapshots are a portable encoding of TransferInputs.
	TransferInputSnapshots []*TransferInputSnapshot

	// InputOutpoints are the VTXO outpoints consumed by this session.
	//
	// This is required to support crash-resilient local persistence after
	// the server accepts finalize.
	InputOutpoints []wire.OutPoint

	// ArkTxid is the Ark txid for confirmation waiting phases.
	ArkTxid *chainhash.Hash

	// RetryAfter is the requested delay in retry backoff phases.
	RetryAfter time.Duration

	// ResumeSnapshot is the snapshot to restore after retry backoff.
	ResumeSnapshot *OutgoingSnapshot

	// FailReason is the terminal failure reason, when Phase is Failed.
	FailReason string
}

// NewOutgoingSnapshot exports an outgoing transfer FSM state into a snapshot.
func NewOutgoingSnapshot(sessionID SessionID, state State) (*OutgoingSnapshot,
	error) {

	if sessionID == (SessionID{}) {
		return nil, fmt.Errorf("session id must be provided")
	}

	if state == nil {
		return nil, fmt.Errorf("state must be provided")
	}

	snap := &OutgoingSnapshot{
		Version:   3,
		SessionID: sessionID,
	}

	switch s := state.(type) {
	case *AwaitingArkSignatures:
		// Snapshot the deterministic submit package before submit is sent so
		// resume can re-drive Ark signing without rebuilding artifacts.
		snap.Phase = OutgoingPhaseArkSignRequested

		ark, err := psbtutil.Serialize(s.ArkPSBT)
		if err != nil {
			return nil, err
		}
		snap.ArkPSBT = ark

		cps, err := serializePSBTSlice(s.CheckpointPSBTs)
		if err != nil {
			return nil, err
		}
		snap.CheckpointPSBTs = cps
		snap.TransferInputs = s.TransferInputs
		inputSnaps, err := snapshotTransferInputs(s.TransferInputs)
		if err == nil {
			snap.TransferInputSnapshots = inputSnaps
		}
		snap.InputOutpoints = s.InputOutpoints

	case *AwaitingSubmitAccepted:
		// Snapshot the entire submit package because it is the
		// canonical v0 payload, and the natural unit for idempotence.
		//
		// Retries should use byte-identical PSBTs.
		//
		// Storing PSBT bytes rather than Go structs makes this snapshot
		// resilient to later refactors in the PSBT builder.
		snap.Phase = OutgoingPhaseSubmitSent

		ark, err := psbtutil.Serialize(s.ArkPSBT)
		if err != nil {
			return nil, err
		}
		snap.ArkPSBT = ark

		cps, err := serializePSBTSlice(s.CheckpointPSBTs)
		if err != nil {
			return nil, err
		}
		snap.CheckpointPSBTs = cps
		snap.TransferInputs = s.TransferInputs
		inputSnaps, err := snapshotTransferInputs(s.TransferInputs)
		if err == nil {
			snap.TransferInputSnapshots = inputSnaps
		}
		snap.InputOutpoints = s.InputOutpoints

	case *AwaitingCheckpointSignatures:
		// This is the "point-of-no-return" state from the client's
		// perspective: we have received operator signature material.
		//
		// The client must be able to resume signing and finalizing
		// after a crash.
		snap.Phase = OutgoingPhaseCoSigned

		ark, err := psbtutil.Serialize(s.ArkPSBT)
		if err != nil {
			return nil, err
		}
		snap.ArkPSBT = ark

		cps, err := serializePSBTSlice(s.CoSignedCheckpointPSBTs)
		if err != nil {
			return nil, err
		}
		snap.CheckpointPSBTs = cps
		snap.TransferInputs = s.TransferInputs
		inputSnaps, err := snapshotTransferInputs(s.TransferInputs)
		if err == nil {
			snap.TransferInputSnapshots = inputSnaps
		}
		snap.InputOutpoints = s.InputOutpoints

	case *AwaitingFinalizeAccepted:
		// Once finalize is sent, the client should only need the
		// finalized checkpoints (plus Ark PSBT) to retry finalize.
		snap.Phase = OutgoingPhaseFinalizeSent

		ark, err := psbtutil.Serialize(s.ArkPSBT)
		if err != nil {
			return nil, err
		}
		snap.ArkPSBT = ark

		cps, err := serializePSBTSlice(s.FinalCheckpointPSBTs)
		if err != nil {
			return nil, err
		}
		snap.CheckpointPSBTs = cps
		snap.InputOutpoints = s.InputOutpoints

	case *AwaitingLocalVTXOUpdate:
		// This phase is an off-chain bookkeeping step: after the server
		// accepts finalize, the local wallet must update its VTXO store to
		// reflect that the inputs are spent.
		snap.Phase = OutgoingPhaseLocalVTXOUpdate
		snap.InputOutpoints = s.InputOutpoints

	case *Completed:
		// Completed is a terminal state. There is no outbox implied by
		// this state, so resumes are no-ops.
		snap.Phase = OutgoingPhaseCompleted

	case *Failed:
		// Failed is terminal. Retrying is not attempted automatically.
		snap.Phase = OutgoingPhaseFailed
		snap.FailReason = s.Reason

	case *RetryBackoff:
		// RetryBackoff wraps another snapshot to support restart-safe
		// retry.
		// The intent is:
		// 1) store the original "resume snapshot" that is safe to retry
		// 2) store the requested delay and reason for UI/telemetry
		snap.Phase = OutgoingPhaseRetryBackoff
		snap.ResumeSnapshot = s.ResumeSnapshot
		snap.RetryAfter = s.RetryAfter
		snap.FailReason = s.Reason
	default:
		return nil, fmt.Errorf("unsupported outgoing state type: %T",
			state)
	}

	return snap, nil
}

// NewSessionFromSnapshot restores an outgoing transfer session from a snapshot.
func NewSessionFromSnapshot(ctx context.Context,
	snapshot *OutgoingSnapshot) (*Session, error) {

	if snapshot == nil {
		return nil, fmt.Errorf("snapshot must be provided")
	}

	state, err := OutgoingStateFromSnapshot(snapshot)
	if err != nil {
		return nil, err
	}

	env := &Environment{SessionID: snapshot.SessionID}

	fsmCfg := StateMachineCfg{
		Logger:        log.WithPrefix(snapshot.SessionID.LogPrefix()),
		ErrorReporter: newContextErrorReporter(ctx, snapshot.SessionID.LogPrefix()),
		InitialState:  state,
		Env:           env,
	}

	sm := protofsm.NewStateMachine(fsmCfg)
	sm.Start(ctx)

	return &Session{
		ID:  snapshot.SessionID,
		FSM: &sm,
	}, nil
}

// OutgoingStateFromSnapshot converts a snapshot into the corresponding
// concrete state type.
func OutgoingStateFromSnapshot(snapshot *OutgoingSnapshot) (State, error) {
	if snapshot.SessionID == (SessionID{}) {
		return nil, fmt.Errorf("session id must be provided")
	}

	if snapshot.Version == 0 {
		return nil, fmt.Errorf("snapshot version must be provided")
	}

	switch snapshot.Phase {
	case OutgoingPhaseArkSignRequested:
		ark, cps, err := parseOutgoingPSBTs(snapshot.ArkPSBT,
			snapshot.CheckpointPSBTs)
		if err != nil {
			return nil, err
		}

		err = requireSessionIDMatchesArk(snapshot.SessionID, ark)
		if err != nil {
			return nil, err
		}

		inputs, err := restoreTransferInputs(snapshot)
		if err != nil {
			return nil, err
		}

		return &AwaitingArkSignatures{
			InputOutpoints:  snapshot.InputOutpoints,
			ArkPSBT:         ark,
			CheckpointPSBTs: cps,
			TransferInputs:  inputs,
		}, nil

	case OutgoingPhaseSubmitSent:
		ark, cps, err := parseOutgoingPSBTs(snapshot.ArkPSBT,
			snapshot.CheckpointPSBTs)
		if err != nil {
			return nil, err
		}

		err = requireSessionIDMatchesArk(snapshot.SessionID, ark)
		if err != nil {
			return nil, err
		}

		inputs, err := restoreTransferInputs(snapshot)
		if err != nil {
			return nil, err
		}

		return &AwaitingSubmitAccepted{
			InputOutpoints:  snapshot.InputOutpoints,
			ArkPSBT:         ark,
			CheckpointPSBTs: cps,
			TransferInputs:  inputs,
		}, nil

	case OutgoingPhaseCoSigned:
		ark, cps, err := parseOutgoingPSBTs(snapshot.ArkPSBT,
			snapshot.CheckpointPSBTs)
		if err != nil {
			return nil, err
		}

		err = requireSessionIDMatchesArk(snapshot.SessionID, ark)
		if err != nil {
			return nil, err
		}

		inputs, err := restoreTransferInputs(snapshot)
		if err != nil {
			return nil, err
		}

		return &AwaitingCheckpointSignatures{
			SessionID:               snapshot.SessionID,
			InputOutpoints:          snapshot.InputOutpoints,
			ArkPSBT:                 ark,
			CoSignedCheckpointPSBTs: cps,
			TransferInputs:          inputs,
		}, nil

	case OutgoingPhaseFinalizeSent:
		ark, cps, err := parseOutgoingPSBTs(snapshot.ArkPSBT,
			snapshot.CheckpointPSBTs)
		if err != nil {
			return nil, err
		}

		err = requireSessionIDMatchesArk(snapshot.SessionID, ark)
		if err != nil {
			return nil, err
		}

		return &AwaitingFinalizeAccepted{
			SessionID:            snapshot.SessionID,
			InputOutpoints:       snapshot.InputOutpoints,
			ArkPSBT:              ark,
			FinalCheckpointPSBTs: cps,
		}, nil

	case OutgoingPhaseLocalVTXOUpdate:
		if len(snapshot.InputOutpoints) == 0 {
			return nil, fmt.Errorf("input outpoints required")
		}

		return &AwaitingLocalVTXOUpdate{
			SessionID:      snapshot.SessionID,
			InputOutpoints: snapshot.InputOutpoints,
		}, nil

	case OutgoingPhaseRetryBackoff:
		if snapshot.ResumeSnapshot == nil {
			return nil, fmt.Errorf("resume snapshot required")
		}

		after := snapshot.RetryAfter
		if after == 0 {
			after = 1 * time.Second
		}

		return &RetryBackoff{
			ResumeSnapshot: snapshot.ResumeSnapshot,
			RetryAfter:     after,
			Reason:         snapshot.FailReason,
		}, nil

	case OutgoingPhaseCompleted:
		return &Completed{}, nil

	case OutgoingPhaseFailed:
		return &Failed{Reason: snapshot.FailReason}, nil

	default:
		return nil, fmt.Errorf("unknown outgoing phase: %s",
			snapshot.Phase)
	}
}

// snapshotTransferInputs converts transfer inputs into portable snapshots.
func snapshotTransferInputs(inputs []TransferInput) ([]*TransferInputSnapshot,
	error) {

	if len(inputs) == 0 {
		return nil, fmt.Errorf("transfer inputs required")
	}

	out := make([]*TransferInputSnapshot, 0, len(inputs))
	for i := range inputs {
		snap, err := inputs[i].ToSnapshot()
		if err != nil {
			return nil, err
		}

		out = append(out, snap)
	}

	return out, nil
}

// restoreTransferInputs returns transfer inputs from either the in-memory
// representation or the portable snapshots.
func restoreTransferInputs(snapshot *OutgoingSnapshot) ([]TransferInput,
	error) {

	if snapshot == nil {
		return nil, fmt.Errorf("snapshot must be provided")
	}

	if len(snapshot.TransferInputs) != 0 {
		return snapshot.TransferInputs, nil
	}

	if len(snapshot.TransferInputSnapshots) == 0 {
		return nil, fmt.Errorf("transfer inputs required")
	}

	inputs := make([]TransferInput, 0, len(snapshot.TransferInputSnapshots))
	for i := range snapshot.TransferInputSnapshots {
		in, err := TransferInputFromSnapshot(
			snapshot.TransferInputSnapshots[i],
		)
		if err != nil {
			return nil, err
		}

		inputs = append(inputs, in)
	}

	return inputs, nil
}

// parseOutgoingPSBTs parses an Ark PSBT and a list of checkpoint PSBTs.
func parseOutgoingPSBTs(arkRaw []byte, checkpointRaws [][]byte) (*psbt.Packet,
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

// requireSessionIDMatchesArk ensures the snapshot session id matches the Ark
// PSBT txid.
func requireSessionIDMatchesArk(sessionID SessionID, ark *psbt.Packet) error {
	if ark == nil || ark.UnsignedTx == nil {
		return fmt.Errorf("ark psbt must be provided")
	}

	arkTxid := ark.UnsignedTx.TxHash()
	if SessionID(arkTxid) != sessionID {
		return fmt.Errorf("ark txid mismatch with session id")
	}

	return nil
}

// serializePSBTSlice serializes a slice of PSBT packets.
func serializePSBTSlice(pkts []*psbt.Packet) ([][]byte, error) {
	if len(pkts) == 0 {
		return nil, fmt.Errorf("psbt slice must be provided")
	}

	raws := make([][]byte, 0, len(pkts))
	for i := range pkts {
		raw, err := psbtutil.Serialize(pkts[i])
		if err != nil {
			return nil, err
		}
		raws = append(raws, raw)
	}

	return raws, nil
}

// parsePSBTSlice parses a slice of serialized PSBT packets.
func parsePSBTSlice(raws [][]byte) ([]*psbt.Packet, error) {
	if len(raws) == 0 {
		return nil, fmt.Errorf("psbt bytes must be provided")
	}

	pkts := make([]*psbt.Packet, 0, len(raws))
	for i := range raws {
		pkt, err := psbtutil.Parse(raws[i])
		if err != nil {
			return nil, err
		}
		pkts = append(pkts, pkt)
	}

	return pkts, nil
}
