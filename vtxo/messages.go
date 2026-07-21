package vtxo

import (
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/round"
)

// ManagerMsg is the message type accepted by the VTXO Manager actor. This is
// a type alias for actormsg.VTXOManagerMsg so the manager can be registered
// directly with the actormsg service key without generic type mismatch.
// Message types are defined in round/vtxo_messages.go and vtxo/messages.go.
type ManagerMsg = actormsg.VTXOManagerMsg

// ManagerResp is the response type returned by the VTXO Manager actor. This
// is an alias for actormsg.VTXOManagerResp so admission responses defined in
// actormsg can be returned directly from the manager without wrapping. The
// actormsg marker interface enables cross-package service key lookup from
// the wallet.
type ManagerResp = actormsg.VTXOManagerResp

// VTXOTerminatedMsg is a type alias whose canonical definition lives in the
// round package.
type VTXOTerminatedMsg = round.VTXOTerminatedMsg

// VTXOCreatedResp is the response to VTXOCreatedNotification.
type VTXOCreatedResp struct{}

// VTXOManagerResp implements actormsg.VTXOManagerResp marker interface.
func (r *VTXOCreatedResp) VTXOManagerResp() {}

// VTXOsMaterializedResp is the response to VTXOsMaterializedNotification.
type VTXOsMaterializedResp struct{}

// VTXOManagerResp implements actormsg.VTXOManagerResp marker interface.
func (r *VTXOsMaterializedResp) VTXOManagerResp() {}

// VTXOTerminatedResp is the response to VTXOTerminatedMsg.
type VTXOTerminatedResp struct{}

// VTXOManagerResp implements actormsg.VTXOManagerResp marker interface.
func (r *VTXOTerminatedResp) VTXOManagerResp() {}

// VTXOsMaterializedNotification notifies the VTXO manager that VTXOs were
// already durably persisted by another actor and only actor activation remains.
//
// The OOR receive path uses this after materializing incoming VTXOs so the
// manager can spawn one VTXO actor per descriptor without performing another
// store write.
type VTXOsMaterializedNotification struct {
	actor.BaseMessage

	// VTXOs are the descriptors that were already persisted locally.
	VTXOs []*Descriptor
}

// MessageType returns the message type identifier.
func (m *VTXOsMaterializedNotification) MessageType() string {
	return "VTXOsMaterializedNotification"
}

// VTXOManagerMsg implements actormsg.VTXOManagerMsg marker interface.
func (m *VTXOsMaterializedNotification) VTXOManagerMsg() {}

// spendReservationFailedMsg is the manager-internal hop-back for an
// asynchronously delivered spend reservation whose FSM turn failed. The
// detached reserve path observes each child's future via OnComplete on a
// separate goroutine; mutating the manager's in-memory reservation map from
// there would race the manager turn, so the failure is delivered as a
// message and the map entry is dropped on the manager goroutine.
type spendReservationFailedMsg struct {
	actor.BaseMessage

	// Outpoint is the VTXO whose reservation failed.
	Outpoint wire.OutPoint

	// Epoch is the reservation epoch the detached watcher observed when it
	// issued the reserve. The manager drops the in-memory mark only if this
	// still matches the outpoint's current epoch, so a stale failure cannot
	// un-gate a reservation a newer session owns.
	Epoch uint64
}

// MessageType returns the message type identifier.
func (m *spendReservationFailedMsg) MessageType() string {
	return "SpendReservationFailedMsg"
}

// VTXOManagerMsg implements actormsg.VTXOManagerMsg marker interface.
func (m *spendReservationFailedMsg) VTXOManagerMsg() {}

// GetActiveVTXOCountRequest requests the number of active VTXO actors managed
// by the VTXO Manager. This goes through the actor message path to avoid
// requiring synchronization.
type GetActiveVTXOCountRequest struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier.
func (r *GetActiveVTXOCountRequest) MessageType() string {
	return "GetActiveVTXOCountRequest"
}

// VTXOManagerMsg implements actormsg.VTXOManagerMsg marker interface.
func (r *GetActiveVTXOCountRequest) VTXOManagerMsg() {}

// GetActiveVTXOCountResponse returns the count of active VTXO actors.
type GetActiveVTXOCountResponse struct {
	// Count is the number of currently active VTXO actors.
	Count int
}

// VTXOManagerResp implements actormsg.VTXOManagerResp marker interface.
func (r *GetActiveVTXOCountResponse) VTXOManagerResp() {}

// ListLiveDescriptorsRequest requests the descriptors of the VTXOs
// the manager recovered from durable state at Start time. Daemon-local
// subsystems use the response to re-arm per-VTXO state (notably the
// recipient fraud watcher) after restart without taking a direct
// dependency on the VTXO store.
type ListLiveDescriptorsRequest struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier.
func (r *ListLiveDescriptorsRequest) MessageType() string {
	return "ListLiveDescriptorsRequest"
}

// VTXOManagerMsg implements actormsg.VTXOManagerMsg marker interface.
func (r *ListLiveDescriptorsRequest) VTXOManagerMsg() {}

// ListLiveDescriptorsResponse returns the descriptors of the live VTXOs
// the manager recovered at Start.
type ListLiveDescriptorsResponse struct {
	// Descriptors is the snapshot of recovered live descriptors. The
	// slice is caller-owned and safe to mutate.
	Descriptors []*Descriptor
}

// VTXOManagerResp implements actormsg.VTXOManagerResp marker interface.
func (r *ListLiveDescriptorsResponse) VTXOManagerResp() {}

// ReconcileExpiryRequest asks the manager to classify every recovered active
// VTXO against the current synchronized chain height. Waved sends this once on
// startup before advertising wallet readiness; future block subscriptions
// continue normal incremental monitoring.
type ReconcileExpiryRequest struct {
	actor.BaseMessage

	// Height is the authoritative best chain height.
	Height int32
}

// MessageType returns the message type identifier.
func (r *ReconcileExpiryRequest) MessageType() string {
	return "ReconcileExpiryRequest"
}

// VTXOManagerMsg implements actormsg.VTXOManagerMsg marker interface.
func (r *ReconcileExpiryRequest) VTXOManagerMsg() {}

// ReconcileExpiryResponse summarizes the required startup classification.
type ReconcileExpiryResponse struct {
	// Checked is the number of active VTXO actors successfully classified.
	Checked int

	// Expired is the number classified into ExpiredState at this height.
	Expired int

	// LegacyRecovered is the subset of Expired upgraded from the historical
	// terminal Failed representation before active actors were classified.
	LegacyRecovered int
}

// VTXOManagerResp implements actormsg.VTXOManagerResp marker interface.
func (r *ReconcileExpiryResponse) VTXOManagerResp() {}

// ExitOutcome classifies the terminal outcome of a unilateral-exit (unroll)
// job, as reported by the unroll subsystem back to the VTXO manager.
type ExitOutcome uint8

const (
	// ExitOutcomeRecoverable indicates the unroll job failed without any
	// on-chain footprint (no proof or sweep transaction was broadcast),
	// so the VTXO is still live from the operator's perspective and must
	// be rolled back to LiveState.
	ExitOutcomeRecoverable ExitOutcome = iota

	// ExitOutcomeConfirmed indicates the unilateral exit was swept and
	// confirmed on-chain, so the VTXO should be retired to the terminal
	// SpentState.
	ExitOutcomeConfirmed
)

// String returns a human-readable label for the exit outcome.
func (o ExitOutcome) String() string {
	switch o {
	case ExitOutcomeRecoverable:
		return "recoverable"

	case ExitOutcomeConfirmed:
		return "confirmed"

	default:
		return "unknown"
	}
}

// ExitOutcomeNotification informs the VTXO manager of the terminal outcome
// of a unilateral-exit job so it can either recover the VTXO back to
// LiveState (ExitOutcomeRecoverable) or retire it to SpentState
// (ExitOutcomeConfirmed). It is sent by the unroll subsystem when a child
// unroll actor reaches a terminal phase. This is the feedback edge that
// closes the soundness gap in wavelength#602: VTXO lifecycle is gated on
// the unroll job's terminal on-chain outcome rather than the user's intent
// to exit.
type ExitOutcomeNotification struct {
	actor.BaseMessage

	// Outpoint identifies the VTXO whose exit reached a terminal outcome.
	Outpoint wire.OutPoint

	// Outcome classifies the terminal outcome.
	Outcome ExitOutcome

	// Reason carries the unroll failure reason for ExitOutcomeRecoverable,
	// used for logging and the restored VTXO's audit trail.
	Reason string

	// ExitPolicyKind is the exit-spend policy the unroll job ran under. It
	// distinguishes a recovery-only target (a non-standard policy such as a
	// vHTLC refund) from a normal wallet coin (standard timeout or empty).
	// A recoverable failure must NOT relive a recovery-only target into the
	// live coin set: it is a swap-contract output, not spendable liquidity.
	ExitPolicyKind actormsg.ExitPolicyKind
}

// MessageType returns the message type identifier.
func (m *ExitOutcomeNotification) MessageType() string {
	return "ExitOutcomeNotification"
}

// VTXOManagerMsg implements actormsg.VTXOManagerMsg marker interface.
func (m *ExitOutcomeNotification) VTXOManagerMsg() {}

// ExitOutcomeResp is the response to ExitOutcomeNotification.
type ExitOutcomeResp struct{}

// VTXOManagerResp implements actormsg.VTXOManagerResp marker interface.
func (r *ExitOutcomeResp) VTXOManagerResp() {}

// =============================================================================
// Relay messages: VTXO actor → Manager → external actor
// =============================================================================
//
// The VTXO actor routes all outbound signals through the manager rather than
// holding direct references to the round actor or chain resolver. These relay
// messages carry pre-built payloads that the manager unwraps and forwards.

// RelayToRoundMsg wraps a message that the manager should relay to the round
// actor. The payload is already in the round-receivable format so the manager
// just forwards it without transformation.
type RelayToRoundMsg struct {
	actor.BaseMessage

	// Payload is the round-receivable message to relay.
	Payload actormsg.RoundReceivable
}

// VTXOManagerMsg implements actormsg.VTXOManagerMsg marker interface.
func (m *RelayToRoundMsg) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *RelayToRoundMsg) MessageType() string { return "RelayToRoundMsg" }

// RelayToRoundResp is the response for RelayToRoundMsg.
type RelayToRoundResp struct{}

// VTXOManagerResp implements actormsg.VTXOManagerResp marker interface.
func (r *RelayToRoundResp) VTXOManagerResp() {}

// =============================================================================
// Admission message aliases
// =============================================================================
//
// The canonical admission request and response types are defined in actormsg
// so both the wallet and vtxo packages can reference them without creating
// an import cycle (wallet → vtxo → round → wallet). These type aliases
// allow existing vtxo code to reference them without qualification.

// SelectAndReserveSpendRequest is an alias for the canonical type in actormsg.
type SelectAndReserveSpendRequest = actormsg.SelectAndReserveSpendRequest

// SelectedVTXO is an alias for the canonical type in actormsg.
type SelectedVTXO = actormsg.SelectedVTXO

// SelectAndReserveSpendResponse is an alias for the canonical type in
// actormsg.
type SelectAndReserveSpendResponse = actormsg.SelectAndReserveSpendResponse

// ReleaseSpendRequest is an alias for the canonical type in actormsg.
type ReleaseSpendRequest = actormsg.ReleaseSpendRequest

// ReleaseSpendResponse is an alias for the canonical type in actormsg.
type ReleaseSpendResponse = actormsg.ReleaseSpendResponse

// CompleteSpendRequest is an alias for the canonical type in actormsg.
type CompleteSpendRequest = actormsg.CompleteSpendRequest

// CompleteSpendResponse is an alias for the canonical type in actormsg.
type CompleteSpendResponse = actormsg.CompleteSpendResponse

// ReserveForfeitRequest is an alias for the canonical type in actormsg.
type ReserveForfeitRequest = actormsg.ReserveForfeitRequest

// ReserveForfeitResponse is an alias for the canonical type in actormsg.
type ReserveForfeitResponse = actormsg.ReserveForfeitResponse

// ReleaseForfeitRequest is an alias for the canonical type in actormsg.
type ReleaseForfeitRequest = actormsg.ReleaseForfeitRequest

// ReleaseForfeitResponse is an alias for the canonical type in actormsg.
type ReleaseForfeitResponse = actormsg.ReleaseForfeitResponse

// CustomForfeitInput is an alias for the canonical type in actormsg.
type CustomForfeitInput = actormsg.CustomForfeitInput

// ActivateCustomForfeitInputsRequest is an alias for the canonical type in
// actormsg.
//
//nolint:ll // Actor message names intentionally mirror the protocol command.
type ActivateCustomForfeitInputsRequest = actormsg.ActivateCustomForfeitInputsRequest

// ActivateCustomForfeitInputsResponse is an alias for the canonical type in
// actormsg.
//
//nolint:ll // Actor message names intentionally mirror the protocol command.
type ActivateCustomForfeitInputsResponse = actormsg.ActivateCustomForfeitInputsResponse

// DropCustomForfeitInputsRequest is an alias for the canonical type in
// actormsg.
type DropCustomForfeitInputsRequest = actormsg.DropCustomForfeitInputsRequest

// DropCustomForfeitInputsResponse is an alias for the canonical type in
// actormsg.
type DropCustomForfeitInputsResponse = actormsg.DropCustomForfeitInputsResponse

// SelectAndReserveForfeitRequest is an alias for the canonical type in
// actormsg.
type SelectAndReserveForfeitRequest = actormsg.SelectAndReserveForfeitRequest

// SelectAndReserveForfeitResponse is an alias for the canonical type in
// actormsg.
type SelectAndReserveForfeitResponse = actormsg.SelectAndReserveForfeitResponse

// ForceUnrollRequest asks the manager to transition a VTXO into
// UnilateralExitState and trigger unroll through the chain resolver.
type ForceUnrollRequest = actormsg.ForceUnrollRequest

// ForceUnrollResponse confirms the force-unroll request was accepted.
type ForceUnrollResponse = actormsg.ForceUnrollResponse
