package oor

import (
	"crypto/sha256"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
)

// ReceiveState is a sealed interface for all states in the incoming transfer
// FSM.
//
// This FSM is separate from the sender transfer FSM. It exists so a client can
// validate and acknowledge incoming transfers in a restart-friendly way.
type ReceiveState interface {
	protofsm.State[Event, OutboxEvent, *Environment]

	receiveStateSealed()
}

// maxCompletedIncomingEventIDs bounds how many distinct recipient hints a
// single incoming OOR session will track and replay across sequential passes
// (for example, a sender receiving both an asset change output and a Bitcoin
// carrier-return output in the same OOR transfer).
const maxCompletedIncomingEventIDs = 64

// IncomingHintKey uniquely identifies one per-script recipient event hint.
// Operators number recipient_event_id per pk_script (so fresh scripts all
// start at 1), meaning event ID alone collides across distinct scripts in the
// same OOR session.
type IncomingHintKey struct {
	ScriptHash [sha256.Size]byte
	EventID    uint64
}

// NewIncomingHintKey constructs a hint key from a recipient routing pkScript
// and its per-script event ID. Returns the zero key if either is missing.
func NewIncomingHintKey(pkScript []byte, eventID uint64) IncomingHintKey {
	if len(pkScript) == 0 || eventID == 0 {
		return IncomingHintKey{}
	}

	return IncomingHintKey{
		ScriptHash: sha256.Sum256(pkScript),
		EventID:    eventID,
	}
}

// IsZero reports whether the hint key is unset.
func (k IncomingHintKey) IsZero() bool {
	return k.EventID == 0 && k.ScriptHash == ([sha256.Size]byte{})
}

// ReceiveResolving indicates the client durably recorded a lightweight
// incoming-transfer hint and still needs to fetch the full Ark/checkpoint
// package outside the live durable actor transaction.
type ReceiveResolving struct {
	SessionID SessionID

	RecipientPkScript []byte

	RecipientEventID uint64

	// CompletedHints carries per-script hints already completed by earlier
	// receive passes for this session so they remain idempotent.
	CompletedHints []IncomingHintKey

	// MaterializedPlainScripts carries SHA-256 hashes of non-asset output
	// scripts already materialized by earlier passes for this session.
	MaterializedPlainScripts [][sha256.Size]byte

	// ResolveAttempts counts how many times the phase-1 hint resolution has
	// been re-issued for this session without a response. It drives the
	// exponential backoff and the terminal give-up in handleResolveRetry,
	// and is persisted in the snapshot so the bound survives restarts.
	ResolveAttempts uint32
}

// HintKey returns the per-script hint key currently being resolved.
func (s *ReceiveResolving) HintKey() IncomingHintKey {
	if s == nil {
		return IncomingHintKey{}
	}

	return NewIncomingHintKey(s.RecipientPkScript, s.RecipientEventID)
}

// String returns a human-readable representation of ReceiveResolving.
func (s *ReceiveResolving) String() string {
	return "ReceiveResolving"
}

// IsTerminal returns false as ReceiveResolving is not terminal.
func (s *ReceiveResolving) IsTerminal() bool {
	return false
}

// receiveStateSealed marks ReceiveResolving as implementing ReceiveState.
func (s *ReceiveResolving) receiveStateSealed() {}

// ReceiveIdle is the initial state for handling incoming transfers.
type ReceiveIdle struct{}

// String returns a human-readable representation of ReceiveIdle.
func (s *ReceiveIdle) String() string {
	return "ReceiveIdle"
}

// IsTerminal returns false as ReceiveIdle is not terminal.
func (s *ReceiveIdle) IsTerminal() bool {
	return false
}

// receiveStateSealed marks ReceiveIdle as implementing ReceiveState.
func (s *ReceiveIdle) receiveStateSealed() {}

// ReceiveNotified indicates the client has validated and surfaced an incoming
// transfer and is waiting for local materialization to finish.
type ReceiveNotified struct {
	SessionID SessionID

	ArkPSBT *psbt.Packet

	FinalCheckpointPSBTs []*psbt.Packet

	AncestorPackages []PackageArtifact

	// Recipients are the structurally validated Ark outputs plus optional
	// semantic policy metadata needed to materialize custom VTXOs.
	Recipients []ArkRecipientOutput

	// TaprootAssetTransfer carries the sealed Taproot Asset transition
	// packages for an incoming asset OOR transfer until materialization
	// verifies and persists the Ark package on the received VTXO.
	TaprootAssetTransfer *oortx.TaprootAssetTransfer

	// ActiveHint identifies the per-script recipient event currently being
	// processed by this receive pass (zero for direct/legacy transitions).
	ActiveHint IncomingHintKey

	// CompletedHints carries per-script hints already completed by earlier
	// receive passes for this session.
	CompletedHints []IncomingHintKey

	// MaterializedPlainScripts carries SHA-256 hashes of non-asset output
	// scripts already materialized by earlier passes for this session.
	MaterializedPlainScripts [][sha256.Size]byte

	// MetadataAttempts counts how many times the authoritative metadata
	// resolution has failed retryably for this session. It drives the
	// exponential backoff and terminal give-up in handleReceiveOutboxError
	// and is persisted in the snapshot so the bound survives restarts.
	MetadataAttempts uint32
}

// String returns a human-readable representation of ReceiveNotified.
func (s *ReceiveNotified) String() string {
	return "ReceiveNotified"
}

// IsTerminal returns false as ReceiveNotified is not terminal.
func (s *ReceiveNotified) IsTerminal() bool {
	return false
}

// receiveStateSealed marks ReceiveNotified as implementing ReceiveState.
func (s *ReceiveNotified) receiveStateSealed() {}

// ReceiveAwaitingAck indicates incoming VTXOs were materialized locally and the
// client is waiting for ack transport completion.
type ReceiveAwaitingAck struct {
	SessionID SessionID

	// ActiveHint identifies the per-script recipient event whose outputs
	// were just materialized.
	ActiveHint IncomingHintKey

	// CompletedHints carries per-script hints already completed by earlier
	// receive passes for this session.
	CompletedHints []IncomingHintKey

	// MaterializedPlainScripts carries SHA-256 hashes of non-asset output
	// scripts materialized on this session so far.
	MaterializedPlainScripts [][sha256.Size]byte
}

// String returns a human-readable representation of ReceiveAwaitingAck.
func (s *ReceiveAwaitingAck) String() string {
	return "ReceiveAwaitingAck"
}

// IsTerminal returns false as ReceiveAwaitingAck is not terminal.
func (s *ReceiveAwaitingAck) IsTerminal() bool {
	return false
}

// receiveStateSealed marks ReceiveAwaitingAck as implementing ReceiveState.
func (s *ReceiveAwaitingAck) receiveStateSealed() {}

// ReceiveCompleted is the terminal success state for an incoming transfer.
type ReceiveCompleted struct {
	// CompletedHints records the per-script recipient hints processed by
	// this session so distinct owned asset outputs can each run a receive
	// pass while duplicate pushes stay idempotent.
	CompletedHints []IncomingHintKey

	// MaterializedPlainScripts records SHA-256 hashes of plain (non-asset)
	// scripts already materialized on this session so follow-up hints for
	// plain Bitcoin outputs stay idempotent no-ops without reopening.
	MaterializedPlainScripts [][sha256.Size]byte
}

// HasHint reports whether key has already completed on this session.
func (s *ReceiveCompleted) HasHint(key IncomingHintKey) bool {
	if s == nil || key.IsZero() {
		return false
	}
	for _, h := range s.CompletedHints {
		if h == key {
			return true
		}
	}

	return false
}

// HasPlainScript reports whether a non-asset output paying to scriptHash was
// already materialized by a prior pass on this session.
func (s *ReceiveCompleted) HasPlainScript(scriptHash [sha256.Size]byte) bool {
	if s == nil || scriptHash == ([sha256.Size]byte{}) {
		return false
	}
	for _, h := range s.MaterializedPlainScripts {
		if h == scriptHash {
			return true
		}
	}

	return false
}

// String returns a human-readable representation of ReceiveCompleted.
func (s *ReceiveCompleted) String() string {
	return "ReceiveCompleted"
}

// IsTerminal returns true as ReceiveCompleted is terminal.
func (s *ReceiveCompleted) IsTerminal() bool {
	return true
}

// receiveStateSealed marks ReceiveCompleted as implementing ReceiveState.
func (s *ReceiveCompleted) receiveStateSealed() {}

// appendCompletedHint returns a defensive copy of existing with key appended
// when key is non-zero and not already present.
func appendCompletedHint(existing []IncomingHintKey,
	key IncomingHintKey) []IncomingHintKey {

	out := append([]IncomingHintKey(nil), existing...)
	if key.IsZero() {
		return out
	}
	for _, h := range out {
		if h == key {
			return out
		}
	}

	return append(out, key)
}

// appendUniqueScriptHash returns a defensive copy of existing with h appended
// when h is non-zero, not already present, and under the per-session cap.
func appendUniqueScriptHash(existing [][sha256.Size]byte,
	h [sha256.Size]byte) [][sha256.Size]byte {

	out := append([][sha256.Size]byte(nil), existing...)
	if h == ([sha256.Size]byte{}) {
		return out
	}
	for _, cur := range out {
		if cur == h {
			return out
		}
	}
	if len(out) >= maxCompletedIncomingEventIDs {
		return out
	}

	return append(out, h)
}

// activeIncomingHint returns the per-script hint being processed by a
// non-terminal incoming receive state, or the zero key when unset.
func activeIncomingHint(state SessionState) IncomingHintKey {
	switch s := state.(type) {
	case *ReceiveResolving:
		return s.HintKey()

	case *ReceiveNotified:
		return s.ActiveHint

	case *ReceiveAwaitingAck:
		return s.ActiveHint

	default:
		return IncomingHintKey{}
	}
}

// priorCompletedReceiveState returns the ReceiveCompleted state to fall back
// to if a follow-up receive pass fails after an earlier pass already
// succeeded, or nil when this is the session's first pass.
func priorCompletedReceiveState(state SessionState) *ReceiveCompleted {
	var (
		hints   []IncomingHintKey
		scripts [][sha256.Size]byte
	)
	switch s := state.(type) {
	case *ReceiveResolving:
		hints = s.CompletedHints
		scripts = s.MaterializedPlainScripts

	case *ReceiveNotified:
		hints = s.CompletedHints
		scripts = s.MaterializedPlainScripts

	case *ReceiveAwaitingAck:
		hints = s.CompletedHints
		scripts = s.MaterializedPlainScripts

	default:
		return nil
	}
	if len(hints) == 0 {
		return nil
	}

	return &ReceiveCompleted{
		CompletedHints: append([]IncomingHintKey(nil), hints...),
		MaterializedPlainScripts: append(
			[][sha256.Size]byte(nil), scripts...,
		),
	}
}
