package actormsg

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/types"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

// =============================================================================
// Spend admission messages: wallet → Manager → VTXO actors
// =============================================================================
//
// These messages are defined in actormsg (rather than vtxo) so both the wallet
// and vtxo packages can use them without creating an import cycle
// (wallet → vtxo → round → wallet).

// SelectAndReserveSpendRequest asks the VTXO manager to select VTXOs covering
// a target amount and atomically reserve them for an OOR spend. The manager
// runs largest-first coin selection, then Asks each selected VTXO actor to
// process SpendReserveEvent. If any reservation fails, already-reserved
// VTXOs are rolled back.
type SelectAndReserveSpendRequest struct {
	actor.BaseMessage

	// TargetAmount is the minimum total value the selected VTXOs must
	// cover.
	TargetAmount btcutil.Amount

	// MinChangeAmount, when positive, asks selection to avoid a
	// non-zero residual below this amount. Exact spends are still valid.
	MinChangeAmount btcutil.Amount

	// RequiredOutpoints, when non-empty, reserves these exact VTXOs instead
	// of running coin selection. Their total must cover TargetAmount.
	RequiredOutpoints []wire.OutPoint
}

// VTXOManagerMsg implements VTXOManagerMsg marker interface.
func (m *SelectAndReserveSpendRequest) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *SelectAndReserveSpendRequest) MessageType() string {
	return "SelectAndReserveSpendRequest"
}

// SelectedVTXO describes a VTXO that was selected and reserved for an OOR
// spend. This is returned in the SelectAndReserveSpendResponse.
type SelectedVTXO struct {
	// Outpoint is the selected VTXO's outpoint.
	Outpoint wire.OutPoint

	// Amount is the value of this VTXO in satoshis.
	Amount btcutil.Amount

	// PkScript is the output script for this VTXO.
	PkScript []byte
}

// SelectAndReserveSpendResponse returns the VTXOs that were selected and
// reserved for an OOR spend.
type SelectAndReserveSpendResponse struct {
	// SelectedVTXOs is the set of VTXOs reserved for this spend.
	SelectedVTXOs []SelectedVTXO

	// TotalSelected is the sum of all selected VTXO amounts.
	TotalSelected btcutil.Amount
}

// VTXOManagerResp implements the VTXOManagerResp marker interface.
func (r *SelectAndReserveSpendResponse) VTXOManagerResp() {}

// ReleaseSpendRequest releases VTXOs previously reserved for an OOR spend
// back to LiveState. Used when the OOR operation fails or is cancelled.
type ReleaseSpendRequest struct {
	actor.BaseMessage

	// Outpoints identifies the VTXOs to release from spend reservation.
	Outpoints []wire.OutPoint
}

// VTXOManagerMsg implements VTXOManagerMsg marker interface.
func (m *ReleaseSpendRequest) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *ReleaseSpendRequest) MessageType() string {
	return "ReleaseSpendRequest"
}

// ReleaseSpendResponse confirms the spend release.
type ReleaseSpendResponse struct {
	// ReleasedCount is the number of VTXOs successfully released.
	ReleasedCount int
}

// VTXOManagerResp implements the VTXOManagerResp marker interface.
func (r *ReleaseSpendResponse) VTXOManagerResp() {}

// CompleteSpendRequest marks VTXOs as fully spent via an OOR transaction.
// This transitions each VTXO from SpendingState to terminal SpentState.
type CompleteSpendRequest struct {
	actor.BaseMessage

	// Outpoints identifies the VTXOs to mark as spent.
	Outpoints []wire.OutPoint
}

// VTXOManagerMsg implements VTXOManagerMsg marker interface.
func (m *CompleteSpendRequest) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *CompleteSpendRequest) MessageType() string {
	return "CompleteSpendRequest"
}

// CompleteSpendResponse confirms the spend completion.
type CompleteSpendResponse struct {
	// CompletedCount is the number of VTXOs marked as spent.
	CompletedCount int
}

// VTXOManagerResp implements the VTXOManagerResp marker interface.
func (r *CompleteSpendResponse) VTXOManagerResp() {}

// =============================================================================
// Forfeit admission messages: wallet → Manager → VTXO actors
// =============================================================================

// ReserveForfeitRequest asks the manager to reserve specific VTXOs for
// cooperative consumption. The manager Asks each actor to process
// PendingForfeitEvent. If any reservation fails, already-claimed VTXOs
// are rolled back via ForfeitReleasedEvent.
type ReserveForfeitRequest struct {
	actor.BaseMessage

	// Outpoints identifies the VTXOs to reserve for forfeit.
	Outpoints []wire.OutPoint
}

// VTXOManagerMsg implements VTXOManagerMsg marker interface.
func (m *ReserveForfeitRequest) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *ReserveForfeitRequest) MessageType() string {
	return "ReserveForfeitRequest"
}

// ReserveForfeitResponse confirms the forfeit reservation.
type ReserveForfeitResponse struct{}

// VTXOManagerResp implements the VTXOManagerResp marker interface.
func (r *ReserveForfeitResponse) VTXOManagerResp() {}

// ReleaseForfeitRequest releases VTXOs from pending forfeit back to
// LiveState. Used when round registration fails after admission.
type ReleaseForfeitRequest struct {
	actor.BaseMessage

	// Outpoints identifies the VTXOs to release from forfeit.
	Outpoints []wire.OutPoint
}

// VTXOManagerMsg implements VTXOManagerMsg marker interface.
func (m *ReleaseForfeitRequest) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *ReleaseForfeitRequest) MessageType() string {
	return "ReleaseForfeitRequest"
}

// ReleaseForfeitResponse confirms the forfeit release.
type ReleaseForfeitResponse struct {
	// ReleasedCount is the number of VTXOs released.
	ReleasedCount int
}

// VTXOManagerResp implements the VTXOManagerResp marker interface.
func (r *ReleaseForfeitResponse) VTXOManagerResp() {}

// CustomForfeitInput describes a caller-supplied VTXO that is not part of the
// wallet's live coin set but still needs a local VTXO actor to sign the exact
// round forfeit transaction once connector details are known.
type CustomForfeitInput struct {
	// Outpoint identifies the custom VTXO.
	Outpoint wire.OutPoint

	// Amount is the custom VTXO value in satoshis.
	Amount btcutil.Amount

	// PkScript is the script committed to by the custom VTXO.
	PkScript []byte

	// PolicyTemplate is the semantic custom VTXO policy template.
	PolicyTemplate []byte

	// ClientKey is the local key used by this daemon to sign its share of
	// the custom policy.
	ClientKey keychain.KeyDescriptor

	// OperatorKey is the Ark operator key committed to by the policy.
	OperatorKey *btcec.PublicKey

	// RelativeExpiry records the policy's CSV delay for local descriptor
	// accounting. Custom paths supplied by the round request still drive
	// the exact forfeit transaction sequence.
	RelativeExpiry uint32

	// RoundID identifies the round lineage that created this custom
	// VTXO. It lets the temporary PendingForfeit descriptor share the
	// same persistence invariants as ordinary indexed VTXOs.
	RoundID string

	// CommitmentTxID is the commitment tx anchoring this custom VTXO.
	CommitmentTxID chainhash.Hash

	// BatchExpiry is the absolute batch expiry height for the custom
	// VTXO lineage.
	BatchExpiry int32

	// ChainDepth records how many OOR checkpoint hops separate this
	// custom VTXO from its commitment tx.
	ChainDepth int

	// CreatedHeight records the block height where the commitment tx was
	// confirmed.
	CreatedHeight int32

	// Ancestry carries the commitment-tree fragments needed for any later
	// unilateral path.
	Ancestry []types.Ancestry
}

// ActivateCustomForfeitInputsRequest starts temporary PendingForfeit VTXO
// actors for custom inputs before registering a round intent. Inputs that are
// not already known to the wallet are persisted as synthetic signer rows;
// inputs that already have durable VTXO rows are overlaid without changing that
// row.
type ActivateCustomForfeitInputsRequest struct {
	actor.BaseMessage

	// Inputs are the custom VTXOs that need temporary forfeit-signing
	// actors.
	Inputs []CustomForfeitInput
}

// VTXOManagerMsg implements VTXOManagerMsg marker interface.
func (m *ActivateCustomForfeitInputsRequest) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *ActivateCustomForfeitInputsRequest) MessageType() string {
	return "ActivateCustomForfeitInputsRequest"
}

// ActivateCustomForfeitInputsResponse confirms custom actor activation.
type ActivateCustomForfeitInputsResponse struct {
	// ActivatedCount is the number of custom input actors activated.
	ActivatedCount int
}

// VTXOManagerResp implements the VTXOManagerResp marker interface.
func (r *ActivateCustomForfeitInputsResponse) VTXOManagerResp() {}

// DropCustomForfeitInputsRequest removes custom PendingForfeit signer overlays
// that were activated for a round intent that was rejected before signing
// started. Synthetic rows are deleted; pre-existing VTXO rows are retained and
// their ordinary actors are restored from storage.
type DropCustomForfeitInputsRequest struct {
	actor.BaseMessage

	// Outpoints identifies the custom forfeit inputs to drop.
	Outpoints []wire.OutPoint
}

// VTXOManagerMsg implements VTXOManagerMsg marker interface.
func (m *DropCustomForfeitInputsRequest) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *DropCustomForfeitInputsRequest) MessageType() string {
	return "DropCustomForfeitInputsRequest"
}

// DropCustomForfeitInputsResponse confirms custom actor cleanup.
type DropCustomForfeitInputsResponse struct {
	// DroppedCount is the number of custom signer overlays removed.
	DroppedCount int
}

// VTXOManagerResp implements the VTXOManagerResp marker interface.
func (r *DropCustomForfeitInputsResponse) VTXOManagerResp() {}

// =============================================================================
// Atomic cooperative select-and-reserve: wallet → Manager → VTXO actors
// =============================================================================
//
// SelectAndReserveForfeitRequest combines coin selection with cooperative
// reservation in a single atomic operation. This is the directed-send
// counterpart of SelectAndReserveSpendRequest: it selects VTXOs covering
// a target amount and drives each into PendingForfeitState (not
// SpendingState). Without this atomic API, a split select-then-reserve
// flow would re-open the race condition that PR 2's admission model
// was designed to close.

// SelectAndReserveForfeitRequest asks the VTXO manager to select VTXOs
// covering a target amount and atomically reserve them for cooperative
// consumption (PendingForfeitState). The manager runs largest-first
// coin selection, then Asks each selected VTXO actor to process
// PendingForfeitEvent. If any reservation fails, already-reserved
// VTXOs are rolled back via ForfeitReleasedEvent.
type SelectAndReserveForfeitRequest struct {
	actor.BaseMessage

	// TargetAmount is the minimum total value the selected VTXOs must
	// cover.
	TargetAmount btcutil.Amount
}

// VTXOManagerMsg implements VTXOManagerMsg marker interface.
func (m *SelectAndReserveForfeitRequest) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *SelectAndReserveForfeitRequest) MessageType() string {
	return "SelectAndReserveForfeitRequest"
}

// SelectAndReserveForfeitResponse returns the VTXOs that were selected
// and reserved for cooperative consumption.
type SelectAndReserveForfeitResponse struct {
	// SelectedVTXOs is the set of VTXOs reserved for this cooperative
	// operation.
	SelectedVTXOs []SelectedVTXO

	// TotalSelected is the sum of all selected VTXO amounts.
	TotalSelected btcutil.Amount
}

// VTXOManagerResp implements the VTXOManagerResp marker interface.
func (r *SelectAndReserveForfeitResponse) VTXOManagerResp() {}

// UnrollTrigger names why a unilateral exit was started. It is a
// string-typed mirror of the unroll package's StartTrigger so the vtxo and
// actormsg packages can carry the trigger through the ForceUnroll path
// without importing unroll (which would form a cycle). The waved chain
// resolver bridge converts these back into unroll.StartTrigger values at the
// seam where both packages are already in scope. The empty string is the
// default and preserves the historical critical-expiry admission.
type UnrollTrigger string

const (
	// UnrollTriggerCriticalExpiry marks an exit driven by a VTXO
	// approaching its batch expiry. It is the zero-value default so an
	// unset trigger keeps admitting as critical expiry, matching the
	// behavior before triggers were threaded end-to-end.
	UnrollTriggerCriticalExpiry UnrollTrigger = ""

	// UnrollTriggerManual marks an operator- or subsystem-requested exit
	// that follows the standard VTXO timeout sweep policy (manual RPC exit,
	// vHTLC refund recovery).
	UnrollTriggerManual UnrollTrigger = "manual"

	// UnrollTriggerFraudSpend marks an exit forced because a watched
	// ancestor of an OOR VTXO was seen spent on-chain. It changes the
	// unroll FSM's CSV handling, so it must survive the whole ForceUnroll
	// path rather than being flattened to the default.
	UnrollTriggerFraudSpend UnrollTrigger = "fraud_spend"
)

// ExitPolicyKind names a durable exit-spend policy for a forced unilateral
// exit. It is a string-typed enum mirroring the unroll / vhtlcrecovery policy
// vocabulary so the vtxo and actormsg packages can carry a policy through the
// ForceUnroll path without importing unroll (a cycle: unroll already imports
// vtxo). The waved chain resolver bridge converts it back into an
// unroll.ExitPolicyKind at the seam where both packages are in scope.
//
// The standard timeout policy is represented by a None fn.Option[ExitPolicy]
// rather than a distinct kind, so the constants below enumerate only the
// non-standard policies that actually ride the ForceUnroll path.
type ExitPolicyKind string

const (
	// ExitPolicyVHTLCClaim identifies the vHTLC unilateral claim leaf
	// spend, mirroring vhtlcrecovery.ExitPolicyKindClaim.
	ExitPolicyVHTLCClaim ExitPolicyKind = "vhtlc_claim"

	// ExitPolicyVHTLCRefundWithoutReceiver identifies the vHTLC unilateral
	// refund-without-receiver leaf spend, mirroring
	// vhtlcrecovery.ExitPolicyKindRefundWithoutReceiver.
	ExitPolicyVHTLCRefundWithoutReceiver ExitPolicyKind = "vhtlc_" +
		"refund_without_receiver"

	// ExitPolicyVirtualChannelBacking identifies the cooperative spend from
	// a VTXO into its already negotiated Lightning channel point.
	ExitPolicyVirtualChannelBacking = ExitPolicyKind(
		"virtual_channel_backing",
	)
)

// Valid reports whether the exit policy kind is one of the known non-standard
// policies that can ride the ForceUnroll path.
func (k ExitPolicyKind) Valid() bool {
	switch k {
	case ExitPolicyVHTLCClaim, ExitPolicyVHTLCRefundWithoutReceiver,
		ExitPolicyVirtualChannelBacking:
		return true

	default:
		return false
	}
}

// ExitPolicyRef is the policy-specific durable reference paired with an
// ExitPolicyKind (e.g. the vHTLC recovery job id). It is a distinct type so
// the Kind and Ref of a policy identity can't be transposed by accident.
type ExitPolicyRef string

// ExitPolicy bundles a non-standard exit-spend policy kind with its durable
// reference. The registry admission boundary validates the pair as a single
// identity, so they travel together. A None fn.Option[ExitPolicy] selects the
// standard VTXO timeout policy.
type ExitPolicy struct {
	// Kind names the durable spend policy.
	Kind ExitPolicyKind

	// Ref is the policy-specific durable reference.
	Ref ExitPolicyRef
}

// ForceUnrollRequest asks the VTXO manager to transition a specific VTXO
// into UnilateralExitState and trigger unroll through the chain resolver
// seam. This converges manual, critical-expiry, fraud, and vHTLC-recovery
// unroll on the same admission path: the manager owns the state transition
// for every trigger, so the coin is persisted UnilateralExit (out of the
// live set) before the unroll registry admits the job.
type ForceUnrollRequest struct {
	actor.BaseMessage

	// Outpoint identifies the VTXO to force-unroll.
	Outpoint wire.OutPoint

	// Reason explains why the unroll was requested.
	Reason string

	// Trigger identifies why the unroll was requested so the chain
	// resolver bridge can admit the registry job under the right
	// StartTrigger. The zero value admits as critical expiry.
	Trigger UnrollTrigger

	// ExitPolicy carries a non-standard exit-spend policy identity (e.g. a
	// vHTLC refund policy) to persist for this target. None selects the
	// standard VTXO timeout policy.
	ExitPolicy fn.Option[ExitPolicy]
}

// VTXOManagerMsg implements VTXOManagerMsg marker interface.
func (m *ForceUnrollRequest) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *ForceUnrollRequest) MessageType() string {
	return "ForceUnrollRequest"
}

// ForceUnrollResponse confirms that the VTXO was transitioned to
// UnilateralExitState and the chain resolver was notified.
type ForceUnrollResponse struct {
	// Accepted is true if the VTXO was successfully transitioned into
	// UnilateralExitState by this request.
	Accepted bool

	// Reason carries a human-readable explanation when Accepted is
	// false (e.g. "no such vtxo", "already terminal"). Empty when
	// Accepted is true.
	Reason string
}

// VTXOManagerResp implements the VTXOManagerResp marker interface.
func (r *ForceUnrollResponse) VTXOManagerResp() {}
