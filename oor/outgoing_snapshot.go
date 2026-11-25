package oor

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
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
// in wavelength will eventually provide a canonical encoding for these.
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

	// CheckpointPSBTs are checkpoint PSBTs relevant for the current
	// phase (serialized).
	CheckpointPSBTs [][]byte

	// TransferInputs are kept for in-process state handling.
	// Durable snapshot encoding uses TransferInputSnapshots.
	TransferInputs []TransferInput

	// TransferInputSnapshots are a portable encoding of
	// TransferInputs.
	TransferInputSnapshots []*TransferInputSnapshot

	// RecipientOutputs are the canonical submit recipients required for a
	// byte-identical submit retry after restart (including asset metadata).
	RecipientOutputs []oortx.RecipientOutput

	// TaprootAssetTransfer is the encoded sealed-package container for an
	// asset-bearing transfer.
	TaprootAssetTransfer []byte

	// RetryAfter is the requested delay for a pending retry, if any.
	RetryAfter time.Duration

	// FailReason is the terminal failure reason when Phase is Failed. When
	// RetryAfter is non-zero, it carries the pending retry reason.
	FailReason string

	// FailedPrePONR records, when Phase is Failed, that the session failed
	// before the point of no return. It decodes to false for a snapshot
	// that lacks the record.
	FailedPrePONR bool

	// IdempotencyKey identifies the caller intent that created this
	// outgoing session, when one was provided.
	IdempotencyKey string

	// DispatchRequestData is the normalized caller-recipient proof retained
	// until the first submit-capable checkpoint commits it separately.
	DispatchRequestData []byte

	// FirstRejectUnixNanos is the Unix-nanosecond timestamp of the first
	// transient submit rejection observed in the AwaitingSubmitAccepted
	// (OutgoingPhaseSubmitSent) phase. It bounds the cumulative transient
	// submit-reject retry window across restarts. Zero means no transient
	// reject has been seen yet; it is also the value a pre-v5 snapshot
	// (which lacks the record) restores to, preserving a fresh retry
	// window.
	FirstRejectUnixNanos int64
}

// outgoingSnapshotVersion is the highest outgoing snapshot format version
// produced and understood by this build.
const outgoingSnapshotVersion uint8 = 7

// NewOutgoingSnapshot exports an outgoing transfer FSM state into a snapshot.
func NewOutgoingSnapshot(sessionID SessionID,
	state State) (*OutgoingSnapshot, error) {

	if sessionID == (SessionID{}) {
		return nil, fmt.Errorf("session id must be provided")
	}

	if state == nil {
		return nil, fmt.Errorf("state must be provided")
	}

	snap := &OutgoingSnapshot{
		// Version 5 adds the FirstRejectUnixNanos record (bounded
		// transient submit-reject retry window). Restore stays
		// backward-compatible: a pre-v5 snapshot lacks the record and
		// decodes FirstRejectUnixNanos to 0 (a fresh window). Version 6
		// adds the FailedPrePONR record, which a pre-v6 snapshot lacks
		// and decodes to false (not proven pre-PONR). Version 7 adds
		// optional RecipientOutputs, TaprootAssetTransfer, and
		// per-input Taproot Asset / OperatorFunded records.
		Version:   outgoingSnapshotVersion,
		SessionID: sessionID,
	}

	switch s := state.(type) {
	case *AwaitingArkSignatures:
		// Snapshot deterministic submit artifacts before submit.
		// This lets resume re-drive Ark signing without rebuilding.
		snap.Phase = OutgoingPhaseArkSignRequested
		snap.IdempotencyKey = s.IdempotencyKey
		snap.DispatchRequestData = append(
			[]byte(nil), s.DispatchRequestData...,
		)

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
		if err != nil {
			return nil, err
		}
		snap.TransferInputSnapshots = inputSnaps
		snap.RecipientOutputs = cloneRecipientOutputs(
			s.RecipientOutputs,
		)

	case *AwaitingSubmitAccepted:
		// Snapshot the entire submit package because it is the
		// canonical v0 payload, and the natural unit for idempotence.
		//
		// Retries should use byte-identical PSBTs.
		//
		// Storing PSBT bytes rather than Go structs makes this snapshot
		// resilient to later refactors in the PSBT builder.
		snap.Phase = OutgoingPhaseSubmitSent
		snap.IdempotencyKey = s.IdempotencyKey
		snap.DispatchRequestData = append(
			[]byte(nil), s.DispatchRequestData...,
		)
		snap.RetryAfter = s.RetryAfter
		snap.FailReason = s.RetryReason
		snap.FirstRejectUnixNanos = s.FirstRejectUnixNanos

		if err := assignPSBTArtifacts(
			snap, s.ArkPSBT, s.CheckpointPSBTs, s.TransferInputs,
		); err != nil {
			return nil, err
		}
		snap.RecipientOutputs = cloneRecipientOutputs(
			s.RecipientOutputs,
		)

	case *AwaitingCheckpointSignatures:
		// This is the "point-of-no-return" state from the client's
		// perspective: we have received operator signature material.
		//
		// The client must be able to resume signing and finalizing
		// after a crash.
		snap.Phase = OutgoingPhaseCoSigned
		snap.IdempotencyKey = s.IdempotencyKey

		if err := assignPSBTArtifacts(
			snap, s.ArkPSBT, s.CoSignedCheckpointPSBTs,
			s.TransferInputs,
		); err != nil {
			return nil, err
		}

	case *AwaitingFinalizeAccepted:
		// Once finalize is sent, the client should only need the
		// finalized checkpoints (plus Ark PSBT) to retry finalize.
		snap.Phase = OutgoingPhaseFinalizeSent
		snap.IdempotencyKey = s.IdempotencyKey

		if err := assignPSBTArtifacts(
			snap, s.ArkPSBT, s.FinalCheckpointPSBTs,
			s.TransferInputs,
		); err != nil {
			return nil, err
		}

	case *AwaitingLocalVTXOUpdate:
		// This phase is an off-chain bookkeeping step: after the server
		// accepts finalize, the local wallet must update local state.
		// That update reflects that the inputs are spent.
		snap.Phase = OutgoingPhaseLocalVTXOUpdate
		snap.IdempotencyKey = s.IdempotencyKey
		err := assignTransferInputSnapshots(snap, s.TransferInputs)
		if err != nil {
			return nil, err
		}

	case *Completed:
		// Completed is a terminal state. There is no outbox implied by
		// this state, so resumes are no-ops.
		snap.Phase = OutgoingPhaseCompleted
		snap.IdempotencyKey = s.IdempotencyKey

	case *Failed:
		// Failed is terminal. Retrying is not attempted automatically.
		snap.Phase = OutgoingPhaseFailed
		snap.FailReason = s.Reason
		snap.FailedPrePONR = s.PrePONR
		snap.IdempotencyKey = s.IdempotencyKey

	default:
		return nil, fmt.Errorf("unsupported outgoing state type: %T",
			state)
	}

	if err := assignSnapshotAssetTransfer(
		snap, stateTaprootAssetTransfer(state),
	); err != nil {
		return nil, err
	}

	return snap, nil
}

// stateTaprootAssetTransfer extracts the optional sealed asset transfer
// container carried by an active outgoing FSM state.
func stateTaprootAssetTransfer(state State) *oortx.TaprootAssetTransfer {
	switch s := state.(type) {
	case *AwaitingArkSignatures:
		return s.TaprootAssetTransfer

	case *AwaitingSubmitAccepted:
		return s.TaprootAssetTransfer

	case *AwaitingCheckpointSignatures:
		return s.TaprootAssetTransfer

	case *AwaitingFinalizeAccepted:
		return s.TaprootAssetTransfer

	case *AwaitingLocalVTXOUpdate:
		return s.TaprootAssetTransfer

	default:
		return nil
	}
}

// NewSessionFromSnapshot restores an outgoing transfer session from a snapshot.
//
// envCfg injects the deterministic clock and the bounded transient
// submit-reject retry budget into the restored FSM Environment so a session
// resumed after a restart keeps the same retry-window semantics as a fresh one.
func NewSessionFromSnapshot(ctx context.Context, snapshot *OutgoingSnapshot,
	envCfg EnvConfig) (*Session, error) {

	if snapshot == nil {
		return nil, fmt.Errorf("snapshot must be provided")
	}

	state, err := OutgoingStateFromSnapshot(snapshot)
	if err != nil {
		return nil, err
	}

	env := envCfg.newEnvironment(snapshot.SessionID)
	baseLogger := logger(ctx)

	fsmCfg := StateMachineCfg{
		Logger: baseLogger.WithPrefix(snapshot.SessionID.LogPrefix()),
		ErrorReporter: newContextErrorReporter(
			ctx, snapshot.SessionID.LogPrefix(),
		),
		InitialState: state,
		Env:          env,
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
	if snapshot.Version > outgoingSnapshotVersion {
		return nil, fmt.Errorf("unsupported snapshot version %d "+
			"(max %d)", snapshot.Version, outgoingSnapshotVersion)
	}

	assetTransfer, err := restoreSnapshotAssetTransfer(
		snapshot.TaprootAssetTransfer,
	)
	if err != nil {
		return nil, err
	}
	if assetTransfer != nil && len(snapshot.CheckpointPSBTs) > 0 {
		if err := assetTransfer.Validate(
			len(snapshot.CheckpointPSBTs),
		); err != nil {
			return nil, err
		}
	}

	switch snapshot.Phase {
	case OutgoingPhaseArkSignRequested:
		ark, cps, inputs, err := restorePSBTPhaseArtifacts(
			snapshot, snapshot.RecipientOutputs, assetTransfer,
		)
		if err != nil {
			return nil, err
		}

		return &AwaitingArkSignatures{
			ArkPSBT:         ark,
			CheckpointPSBTs: cps,
			TransferInputs:  inputs,
			RecipientOutputs: cloneRecipientOutputs(
				snapshot.RecipientOutputs,
			),
			TaprootAssetTransfer: assetTransfer,
			IdempotencyKey:       snapshot.IdempotencyKey,
			DispatchRequestData: append(
				[]byte(nil), snapshot.DispatchRequestData...,
			),
		}, nil

	case OutgoingPhaseSubmitSent:
		ark, cps, inputs, err := restorePSBTPhaseArtifacts(
			snapshot, snapshot.RecipientOutputs, assetTransfer,
		)
		if err != nil {
			return nil, err
		}

		return &AwaitingSubmitAccepted{
			ArkPSBT:         ark,
			CheckpointPSBTs: cps,
			TransferInputs:  inputs,
			RecipientOutputs: cloneRecipientOutputs(
				snapshot.RecipientOutputs,
			),
			TaprootAssetTransfer: assetTransfer,
			IdempotencyKey:       snapshot.IdempotencyKey,
			DispatchRequestData: append(
				[]byte(nil), snapshot.DispatchRequestData...,
			),
			RetryAfter:           snapshot.RetryAfter,
			RetryReason:          snapshot.FailReason,
			FirstRejectUnixNanos: snapshot.FirstRejectUnixNanos,
		}, nil

	case OutgoingPhaseCoSigned:
		ark, cps, inputs, err := restorePSBTPhaseArtifacts(
			snapshot, nil, assetTransfer,
		)
		if err != nil {
			return nil, err
		}

		return &AwaitingCheckpointSignatures{
			SessionID:               snapshot.SessionID,
			ArkPSBT:                 ark,
			CoSignedCheckpointPSBTs: cps,
			TransferInputs:          inputs,
			TaprootAssetTransfer:    assetTransfer,
			IdempotencyKey:          snapshot.IdempotencyKey,
		}, nil

	case OutgoingPhaseFinalizeSent:
		ark, cps, inputs, err := restorePSBTPhaseArtifacts(
			snapshot, nil, assetTransfer,
		)
		if err != nil {
			return nil, err
		}

		return &AwaitingFinalizeAccepted{
			SessionID:            snapshot.SessionID,
			ArkPSBT:              ark,
			FinalCheckpointPSBTs: cps,
			TransferInputs:       inputs,
			TaprootAssetTransfer: assetTransfer,
			IdempotencyKey:       snapshot.IdempotencyKey,
		}, nil

	case OutgoingPhaseLocalVTXOUpdate:
		inputs, err := restoreTransferInputs(snapshot)
		if err != nil {
			return nil, err
		}
		if err := validateRestoredAssetState(
			inputs, nil, assetTransfer,
		); err != nil {
			return nil, err
		}

		return &AwaitingLocalVTXOUpdate{
			SessionID:            snapshot.SessionID,
			TransferInputs:       inputs,
			TaprootAssetTransfer: assetTransfer,
			IdempotencyKey:       snapshot.IdempotencyKey,
		}, nil

	case OutgoingPhaseCompleted:
		return &Completed{
			IdempotencyKey: snapshot.IdempotencyKey,
		}, nil

	case OutgoingPhaseFailed:
		return &Failed{
			Reason:         snapshot.FailReason,
			IdempotencyKey: snapshot.IdempotencyKey,
			PrePONR:        snapshot.FailedPrePONR,
		}, nil

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

// assignPSBTArtifacts serializes the Ark PSBT and the given checkpoint PSBTs
// onto the snapshot and records the portable transfer-input encoding. This is
// the artifact set every submit-through-finalize outgoing phase persists, so it
// is factored out to keep NewOutgoingSnapshot's per-phase cases short.
func assignPSBTArtifacts(snap *OutgoingSnapshot, ark *psbt.Packet,
	checkpoints []*psbt.Packet, inputs []TransferInput) error {

	arkRaw, err := psbtutil.Serialize(ark)
	if err != nil {
		return err
	}
	snap.ArkPSBT = arkRaw

	cps, err := serializePSBTSlice(checkpoints)
	if err != nil {
		return err
	}
	snap.CheckpointPSBTs = cps

	return assignTransferInputSnapshots(snap, inputs)
}

// assignTransferInputSnapshots stores transfer inputs and their portable
// snapshot form on the outgoing snapshot.
func assignTransferInputSnapshots(snap *OutgoingSnapshot,
	inputs []TransferInput) error {

	snap.TransferInputs = inputs

	inputSnaps, err := snapshotTransferInputs(inputs)
	if err != nil {
		return err
	}

	snap.TransferInputSnapshots = inputSnaps

	return nil
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

// assignSnapshotAssetTransfer marshals an optional TaprootAssetTransfer into
// snapshot bytes.
func assignSnapshotAssetTransfer(snapshot *OutgoingSnapshot,
	assetTransfer *oortx.TaprootAssetTransfer) error {

	if assetTransfer == nil {
		return nil
	}

	encoded, err := assetTransfer.MarshalBinary()
	if err != nil {
		return err
	}
	snapshot.TaprootAssetTransfer = encoded

	return nil
}

// restoreSnapshotAssetTransfer unmarshals an optional TaprootAssetTransfer
// from snapshot bytes.
func restoreSnapshotAssetTransfer(encoded []byte) (*oortx.TaprootAssetTransfer,
	error) {

	if len(encoded) == 0 {
		return nil, nil
	}

	assetTransfer := &oortx.TaprootAssetTransfer{}
	if err := assetTransfer.UnmarshalBinary(encoded); err != nil {
		return nil, err
	}

	return assetTransfer, nil
}

// cloneRecipientOutputs returns a deep copy of recipients.
func cloneRecipientOutputs(
	recipients []oortx.RecipientOutput) []oortx.RecipientOutput {

	if len(recipients) == 0 {
		return nil
	}

	cloned := make([]oortx.RecipientOutput, len(recipients))
	for i := range recipients {
		cloned[i] = recipients[i].Clone()
	}

	return cloned
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

// restorePSBTPhaseArtifacts parses and cross-validates the Ark PSBT,
// checkpoint PSBTs, transfer inputs, and asset container shared by all
// PSBT-bearing outgoing snapshot phases.
func restorePSBTPhaseArtifacts(snapshot *OutgoingSnapshot,
	recipients []oortx.RecipientOutput,
	assetTransfer *oortx.TaprootAssetTransfer) (*psbt.Packet,
	[]*psbt.Packet, []TransferInput, error) {

	ark, cps, err := parseOutgoingPSBTs(
		snapshot.ArkPSBT, snapshot.CheckpointPSBTs,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	if err := requireSessionIDMatchesArk(
		snapshot.SessionID, ark,
	); err != nil {
		return nil, nil, nil, err
	}

	inputs, err := restoreTransferInputs(snapshot)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := validateRestoredAssetState(
		inputs, recipients, assetTransfer,
	); err != nil {
		return nil, nil, nil, err
	}

	return ark, cps, inputs, nil
}

// validateRestoredAssetState enforces that restored active sessions with
// Taproot Asset or operator-funded inputs always carry a matching
// TaprootAssetTransfer container, and that plain sessions carry none.
func validateRestoredAssetState(inputs []TransferInput,
	recipients []oortx.RecipientOutput,
	assetTransfer *oortx.TaprootAssetTransfer) error {

	hasAssetInput := false
	hasOperatorFunded := false
	for i := range inputs {
		if inputs[i].OperatorFunded {
			hasOperatorFunded = true
		}
		if inputs[i].VTXO != nil &&
			inputs[i].VTXO.TaprootAssetRoot != nil {

			hasAssetInput = true
		}
	}

	hasAssetRecipient := false
	for i := range recipients {
		if recipients[i].Asset != nil {
			hasAssetRecipient = true
		}
	}

	if assetTransfer == nil {
		if hasAssetInput || hasOperatorFunded || hasAssetRecipient {
			return fmt.Errorf("restored asset or operator-funded " +
				"session requires taproot asset transfer " +
				"container")
		}

		return nil
	}

	if err := assetTransfer.Validate(len(inputs)); err != nil {
		return err
	}
	for i := range inputs {
		hasInputAsset := inputs[i].VTXO != nil &&
			inputs[i].VTXO.TaprootAssetRoot != nil
		hasSlot := len(assetTransfer.CheckpointPackages[i]) > 0
		if hasInputAsset != hasSlot {
			return fmt.Errorf("restored input %d asset presence "+
				"does not match checkpoint package slot", i)
		}
	}

	return nil
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
