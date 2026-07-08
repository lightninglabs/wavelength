package unroll

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// snapshot.go implements the TLV codec for the per-target unroll actor's
// durable checkpoint.
//
// A checkpoint captures every piece of mutable state the actor needs to
// resume its FSM after a restart:
//
//   - Version: gates incompatible schema changes so a newer build
//     refuses to decode rows it cannot reason about.
//   - Height: the best chain height the actor had observed, so a resume
//     starts with non-stale clock data.
//   - Started / Trigger: whether the actor has left Idle, and why it
//     was started (critical-expiry, manual, restart, fraud-spend).
//   - State: the pure [unrollplan.State] payload — confirmed/in-flight
//     txids, target confirm height, sweep status and txid.
//   - SweepTx: the serialized final sweep. Persisted so restart
//     re-submits the exact same bytes to txconfirm (txid-keyed dedup
//     then makes the re-submit a benign no-op). Without byte-exact
//     restoration we would risk broadcasting a differently-signed
//     sweep with a fresh wallet pkScript.
//   - Fail: the FailReason if the actor has reached terminal failure.
//   - SweepAttempts: number of sweep build/broadcast failures so far,
//     compared against maxSweepAttempts when deciding whether to keep
//     retrying.
//   - DeferredCheckpoints: fraud-triggered checkpoint nodes that are being
//     watched for operator confirmation before the recipient backstop.
//   - ExitPolicyKind / ExitPolicyRef: the durable identity of the policy
//     used to build the final exit spend.
//
// TLV was picked over JSON for three reasons: schema evolution (new
// optional records slot in without breaking old readers), determinism
// (canonical ordering by record type means identical states encode to
// identical bytes — useful for equality checks and diffing), and
// compactness (sweep tx stays in its native wire format).

const (
	checkpointStateType = "unroll.vtxo"
	checkpointVersion   = 1
)

// Outer record types for the actor checkpoint TLV stream. Odd type values are
// used throughout so that future extensions can slot even types in without
// breaking canonical encoding ordering.
const (
	// checkpointVersionRecordType carries the codec version byte.
	checkpointVersionRecordType tlv.Type = 1

	// checkpointHeightRecordType carries the best height tracked by the
	// actor at checkpoint time.
	checkpointHeightRecordType tlv.Type = 3

	// checkpointStartedRecordType carries a 1-byte bool indicating whether
	// the actor has started (i.e. left the Idle state).
	checkpointStartedRecordType tlv.Type = 5

	// checkpointTriggerRecordType carries the start trigger enum.
	checkpointTriggerRecordType tlv.Type = 7

	// checkpointStateRecordType carries the nested planner state bytes
	// produced by unrollplan.EncodeState.
	checkpointStateRecordType tlv.Type = 9

	// checkpointSweepTxRecordType is optional; present only when a sweep
	// transaction has been built. Payload is wire.MsgTx.Serialize bytes.
	checkpointSweepTxRecordType tlv.Type = 11

	// checkpointFailRecordType is optional; present only when the actor
	// has recorded a failure reason.
	checkpointFailRecordType tlv.Type = 13

	// checkpointSweepAttemptsRecordType carries the cumulative count of
	// sweep-build attempts.
	checkpointSweepAttemptsRecordType tlv.Type = 15

	// checkpointDeferredCheckpointsRecordType carries fraud-triggered
	// checkpoint deferrals.
	checkpointDeferredCheckpointsRecordType tlv.Type = 17

	// checkpointExitPolicyKindRecordType carries the policy kind used to
	// reconstruct the final exit spend after restart.
	checkpointExitPolicyKindRecordType tlv.Type = 19

	// checkpointExitPolicyRefRecordType carries the policy-specific
	// durable-state reference.
	checkpointExitPolicyRefRecordType tlv.Type = 21

	// checkpointExternalSpendRecordType is optional; present only when
	// the actor has observed an external spend of the target outpoint
	// that has not yet been finalized. Payload is a fixed-layout
	// 36-byte blob (32-byte SpendingTxid + 4-byte big-endian
	// SpendingHeight) so a daemon restarting mid-spend-finality-window
	// can rehydrate into AwaitingExternalSpendFinality instead of
	// dropping the provisional anchor and broadcasting a sweep on a
	// target the chain says no longer exists.
	checkpointExternalSpendRecordType tlv.Type = 23

	// checkpointSweepFinalizedRecordType is optional; present (value 1)
	// only when the sweep has finalized past the backend's reorg-safety
	// depth. It makes the in-memory sweep-finalized latch durable so a
	// PhaseCompleted entry whose terminal handoff was deferred or failed
	// does not restart as permanently "provisional completed" (the latch
	// gates notifyRegistryIfTerminal). Omitted when false.
	checkpointSweepFinalizedRecordType tlv.Type = 25
)

// externalSpendBlobSize is the canonical wire size of the persisted
// ProvisionalExternalSpend payload: 32-byte txid + 4-byte big-endian
// height.
const externalSpendBlobSize = chainhash.HashSize + 4

// actorCheckpoint is the durable checkpoint shape for one VTXO unroll actor.
type actorCheckpoint struct {
	Version                  uint8
	Height                   int32
	Started                  bool
	Trigger                  StartTrigger
	State                    unrollplan.State
	ExitPolicyKind           ExitPolicyKind
	ExitPolicyRef            string
	SweepTx                  *wire.MsgTx
	Fail                     string
	SweepAttempts            int
	DeferredCheckpoints      []DeferredCheckpoint
	ProvisionalExternalSpend fn.Option[ExternalSpendAnchor]
	SweepFinalized           bool
}

// encodeExternalSpendBlob serializes an ExternalSpendAnchor into the
// fixed-layout payload carried by checkpointExternalSpendRecordType.
func encodeExternalSpendBlob(anchor ExternalSpendAnchor) []byte {
	blob := make([]byte, externalSpendBlobSize)
	copy(blob[:chainhash.HashSize], anchor.SpendingTxid[:])
	binary.BigEndian.PutUint32(
		blob[chainhash.HashSize:], uint32(anchor.SpendingHeight),
	)

	return blob
}

// decodeExternalSpendBlob parses a fixed-layout external-spend payload
// back into an ExternalSpendAnchor.
func decodeExternalSpendBlob(blob []byte) (ExternalSpendAnchor, error) {
	if len(blob) != externalSpendBlobSize {
		return ExternalSpendAnchor{}, fmt.Errorf("external spend blob "+
			"has %d bytes, want %d", len(blob),
			externalSpendBlobSize)
	}

	var anchor ExternalSpendAnchor
	copy(anchor.SpendingTxid[:], blob[:chainhash.HashSize])
	anchor.SpendingHeight = int32(
		binary.BigEndian.Uint32(blob[chainhash.HashSize:]),
	)

	return anchor, nil
}

// encodeCheckpoint serializes one actor checkpoint into canonical TLV
// bytes.
//
// Canonical here means: records always appear in ascending type order
// (enforced by tlv.Stream) and optional fields are omitted entirely
// when empty. This is what lets us compare "has anything changed since
// last checkpoint?" by byte equality in the registry's pending/persisted
// divergence check — two semantically-identical states encode to
// identical bytes.
//
// The sweep tx is serialized via wire.MsgTx.Serialize so the stored
// bytes are directly re-playable; we never re-derive the tx on restart.
func encodeCheckpoint(value *actorCheckpoint) ([]byte, error) {
	if value == nil {
		return nil, fmt.Errorf("checkpoint cannot be nil")
	}

	version := value.Version
	height := uint32(value.Height)
	started := uint8(0)
	if value.Started {
		started = 1
	}
	trigger := uint32(value.Trigger)
	attempts := uint32(value.SweepAttempts)

	stateBytes, err := unrollplan.EncodeState(&value.State)
	if err != nil {
		return nil, fmt.Errorf("encode planner state: %w", err)
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			checkpointVersionRecordType, &version,
		),
		tlv.MakePrimitiveRecord(
			checkpointHeightRecordType, &height,
		),
		tlv.MakePrimitiveRecord(
			checkpointStartedRecordType, &started,
		),
		tlv.MakePrimitiveRecord(
			checkpointTriggerRecordType, &trigger,
		),
		tlv.MakePrimitiveRecord(
			checkpointStateRecordType, &stateBytes,
		),
	}

	if value.SweepTx != nil {
		var sweepBuf bytes.Buffer
		if err := value.SweepTx.Serialize(&sweepBuf); err != nil {
			return nil, fmt.Errorf("serialize sweep tx: %w", err)
		}
		sweepBytes := sweepBuf.Bytes()
		records = append(
			records, tlv.MakePrimitiveRecord(
				checkpointSweepTxRecordType, &sweepBytes,
			),
		)
	}

	if value.Fail != "" {
		failBytes := []byte(value.Fail)
		records = append(
			records, tlv.MakePrimitiveRecord(
				checkpointFailRecordType, &failBytes,
			),
		)
	}

	records = append(
		records, tlv.MakePrimitiveRecord(
			checkpointSweepAttemptsRecordType, &attempts,
		),
	)

	if len(value.DeferredCheckpoints) > 0 {
		deferredBytes, err := encodeDeferredCheckpoints(
			value.DeferredCheckpoints,
		)
		if err != nil {
			return nil, fmt.Errorf("encode deferred "+
				"checkpoints: %w", err)
		}
		records = append(
			records, tlv.MakePrimitiveRecord(
				checkpointDeferredCheckpointsRecordType,
				&deferredBytes,
			),
		)
	}

	policyKind := exitPolicyKind(value.ExitPolicyKind)
	if policyKind != StandardVTXOTimeoutExitPolicyKind ||
		value.ExitPolicyRef != "" {

		policyKindBytes := []byte(policyKind)
		records = append(
			records, tlv.MakePrimitiveRecord(
				checkpointExitPolicyKindRecordType,
				&policyKindBytes,
			),
		)
	}

	if value.ExitPolicyRef != "" {
		policyRefBytes := []byte(value.ExitPolicyRef)
		records = append(
			records, tlv.MakePrimitiveRecord(
				checkpointExitPolicyRefRecordType,
				&policyRefBytes,
			),
		)
	}

	value.ProvisionalExternalSpend.WhenSome(
		func(anchor ExternalSpendAnchor) {
			blob := encodeExternalSpendBlob(anchor)
			records = append(
				records, tlv.MakePrimitiveRecord(
					checkpointExternalSpendRecordType,
					&blob,
				),
			)
		},
	)

	if value.SweepFinalized {
		finalized := uint8(1)
		records = append(
			records, tlv.MakePrimitiveRecord(
				checkpointSweepFinalizedRecordType, &finalized,
			),
		)
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, fmt.Errorf("create checkpoint stream: %w", err)
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode checkpoint: %w", err)
	}

	return buf.Bytes(), nil
}

// decodeCheckpoint parses TLV-encoded checkpoint bytes back into an
// actorCheckpoint.
//
// The decoder distinguishes missing-but-optional records (SweepTx, Fail)
// from missing-but-required records (Version) by consulting the parsed
// types map from tlv.Stream.DecodeWithParsedTypes. A missing Version
// field is a hard error: without it we cannot rule out rows written by
// an older schema that would silently decode into a partially populated
// struct.
//
// Unknown Version values are also rejected. A newer daemon starting
// against a store written by an even newer future daemon refuses to
// guess at forward-compat semantics, rather than quietly operating on
// truncated state.
func decodeCheckpoint(raw []byte) (*actorCheckpoint, error) {
	var (
		version        uint8
		height         uint32
		started        uint8
		trigger        uint32
		stateBytes     []byte
		sweepBytes     []byte
		failBytes      []byte
		attempts       uint32
		deferredBytes  []byte
		policyKind     []byte
		policyRef      []byte
		extSpendBlob   []byte
		sweepFinalized uint8
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			checkpointVersionRecordType, &version,
		),
		tlv.MakePrimitiveRecord(
			checkpointHeightRecordType, &height,
		),
		tlv.MakePrimitiveRecord(
			checkpointStartedRecordType, &started,
		),
		tlv.MakePrimitiveRecord(
			checkpointTriggerRecordType, &trigger,
		),
		tlv.MakePrimitiveRecord(
			checkpointStateRecordType, &stateBytes,
		),
		tlv.MakePrimitiveRecord(
			checkpointSweepTxRecordType, &sweepBytes,
		),
		tlv.MakePrimitiveRecord(
			checkpointFailRecordType, &failBytes,
		),
		tlv.MakePrimitiveRecord(
			checkpointSweepAttemptsRecordType, &attempts,
		),
		tlv.MakePrimitiveRecord(
			checkpointDeferredCheckpointsRecordType, &deferredBytes,
		),
		tlv.MakePrimitiveRecord(
			checkpointExitPolicyKindRecordType, &policyKind,
		),
		tlv.MakePrimitiveRecord(
			checkpointExitPolicyRefRecordType, &policyRef,
		),
		tlv.MakePrimitiveRecord(
			checkpointExternalSpendRecordType, &extSpendBlob,
		),
		tlv.MakePrimitiveRecord(
			checkpointSweepFinalizedRecordType, &sweepFinalized,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create checkpoint stream: %w", err)
	}

	parsed, err := stream.DecodeWithParsedTypes(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode checkpoint: %w", err)
	}

	if _, ok := parsed[checkpointVersionRecordType]; !ok {
		return nil, fmt.Errorf("checkpoint missing version record")
	}
	if version != checkpointVersion {
		return nil, fmt.Errorf("unsupported checkpoint version %d "+
			"(expected %d)", version, checkpointVersion)
	}

	state, err := unrollplan.DecodeState(stateBytes)
	if err != nil {
		return nil, fmt.Errorf("decode planner state: %w", err)
	}

	checkpoint := &actorCheckpoint{
		Version:        version,
		Height:         int32(height),
		Started:        started != 0,
		Trigger:        StartTrigger(int32(trigger)),
		State:          *state,
		ExitPolicyKind: StandardVTXOTimeoutExitPolicyKind,
		SweepAttempts:  int(attempts),
	}

	if _, ok := parsed[checkpointExitPolicyKindRecordType]; ok {
		checkpoint.ExitPolicyKind = exitPolicyKind(
			ExitPolicyKind(policyKind),
		)
	}

	if _, ok := parsed[checkpointExitPolicyRefRecordType]; ok {
		checkpoint.ExitPolicyRef = string(policyRef)
	}

	if _, ok := parsed[checkpointSweepTxRecordType]; ok {
		tx := wire.NewMsgTx(0)
		err := tx.Deserialize(bytes.NewReader(sweepBytes))
		if err != nil {
			return nil, fmt.Errorf("deserialize sweep tx: %w", err)
		}
		checkpoint.SweepTx = tx
	}

	if _, ok := parsed[checkpointFailRecordType]; ok {
		checkpoint.Fail = string(failBytes)
	}

	if _, ok := parsed[checkpointDeferredCheckpointsRecordType]; ok {
		checkpoints, err := decodeDeferredCheckpoints(deferredBytes)
		if err != nil {
			return nil, fmt.Errorf("decode deferred "+
				"checkpoints: %w", err)
		}
		checkpoint.DeferredCheckpoints = checkpoints
	}

	if _, ok := parsed[checkpointExternalSpendRecordType]; ok {
		anchor, err := decodeExternalSpendBlob(extSpendBlob)
		if err != nil {
			return nil, fmt.Errorf("decode external spend: %w", err)
		}
		checkpoint.ProvisionalExternalSpend = fn.Some(anchor)
	}

	if _, ok := parsed[checkpointSweepFinalizedRecordType]; ok {
		checkpoint.SweepFinalized = sweepFinalized != 0
	}

	return checkpoint, nil
}

// exitPolicyKind returns the standard timeout policy when callers have not
// supplied an explicit custom policy kind.
func exitPolicyKind(kind ExitPolicyKind) ExitPolicyKind {
	if kind == "" {
		return StandardVTXOTimeoutExitPolicyKind
	}

	return kind
}
