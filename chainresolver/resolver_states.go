package chainresolver

import (
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/db"
)

// ResolverState is a sealed interface for all states in the per-VTXO resolver
// state machine. Each state implements ProcessEvent to handle incoming events
// and return state transitions.
type ResolverState interface {
	protofsm.State[ResolverEvent, ResolverOutMsg, *ResolverEnvironment]

	// resolverStateSealed is an unexported method that prevents external
	// packages from implementing ResolverState.
	resolverStateSealed()
}

// ResolverStateTransition is a type alias for the verbose
// protofsm.StateTransition type used throughout the resolver FSM.
type ResolverStateTransition = protofsm.StateTransition[
	ResolverEvent, ResolverOutMsg, *ResolverEnvironment,
]

// ResolverEmittedEvent is a type alias for the verbose protofsm.EmittedEvent
// type used when state transitions emit new events or outbox messages.
type ResolverEmittedEvent = protofsm.EmittedEvent[
	ResolverEvent, ResolverOutMsg,
]

// WatchingCommitmentState is the initial state for fraud-reactive resolvers.
// The resolver watches the batch outpoint for a spend by another party. When
// a spend is detected, the resolver inspects the spending transaction to
// determine which tree level is already on-chain and transitions to
// BroadcastingTreeState with that level pre-confirmed.
type WatchingCommitmentState struct {
	// Trigger is the reason this resolution was initiated.
	Trigger ResolveTrigger

	// Outpoint is the VTXO outpoint being resolved.
	Outpoint wire.OutPoint
}

// String returns a human-readable state name.
func (s *WatchingCommitmentState) String() string {
	return "WatchingCommitment"
}

// IsTerminal returns false since WatchingCommitmentState is not terminal.
func (s *WatchingCommitmentState) IsTerminal() bool {
	return false
}

// resolverStateSealed implements the sealed ResolverState interface.
func (s *WatchingCommitmentState) resolverStateSealed() {}

// BroadcastingTreeState is the state where the resolver broadcasts the
// virtual transaction tree level by level. Each level must confirm before
// the next can be broadcast. For expiry and user-initiated triggers, this
// is the initial state. For fraud-reactive triggers, this state is entered
// after a spend is detected on the batch outpoint.
type BroadcastingTreeState struct {
	// Outpoint is the VTXO outpoint being resolved.
	Outpoint wire.OutPoint

	// Trigger is the reason this resolution was initiated.
	Trigger ResolveTrigger

	// CurrentLevel is the tree level being broadcast (0 = root).
	CurrentLevel int

	// MaxLevel is the maximum tree level that needs broadcasting.
	MaxLevel int

	// ConfirmedLevels is the number of tree levels confirmed so far.
	ConfirmedLevels int

	// AlreadyOnChainLevel is the tree level that was already found
	// on-chain (fraud-reactive path). Set to -1 when not applicable.
	AlreadyOnChainLevel int
}

// String returns a human-readable state name.
func (s *BroadcastingTreeState) String() string {
	return fmt.Sprintf(
		"BroadcastingTree(level=%d/%d, confirmed=%d)",
		s.CurrentLevel, s.MaxLevel, s.ConfirmedLevels,
	)
}

// IsTerminal returns false since BroadcastingTreeState is not terminal.
func (s *BroadcastingTreeState) IsTerminal() bool {
	return false
}

// resolverStateSealed implements the sealed ResolverState interface.
func (s *BroadcastingTreeState) resolverStateSealed() {}

// BroadcastingCheckpointsState is the state where the resolver broadcasts
// OOR checkpoint transactions. Checkpoints are broadcast in order from the
// farthest ancestor to the target package, with a CSV delay wait between
// each. This state is only entered for VTXOs that have OOR packages.
type BroadcastingCheckpointsState struct {
	// Outpoint is the VTXO outpoint being resolved.
	Outpoint wire.OutPoint

	// Packages is the ordered list of OOR package bundles to broadcast.
	// Ordered from farthest ancestor to target package.
	Packages []*db.OORPackageBundle

	// CurrentPackageIdx is the index into Packages of the checkpoint
	// being broadcast or awaiting CSV maturity.
	CurrentPackageIdx int

	// CSVDelay is the relative CSV delay (in blocks) between checkpoint
	// confirmations.
	CSVDelay uint32

	// LastConfHeight is the block height at which the last checkpoint
	// confirmed. Used to calculate when the CSV delay has elapsed.
	// Set to -1 when no checkpoint has confirmed yet.
	LastConfHeight int32

	// WaitingForCSV indicates whether the resolver is waiting for the
	// CSV delay to elapse before broadcasting the next checkpoint.
	WaitingForCSV bool
}

// String returns a human-readable state name.
func (s *BroadcastingCheckpointsState) String() string {
	return fmt.Sprintf(
		"BroadcastingCheckpoints(pkg=%d/%d, csv_wait=%v)",
		s.CurrentPackageIdx, len(s.Packages), s.WaitingForCSV,
	)
}

// IsTerminal returns false since BroadcastingCheckpointsState is not
// terminal.
func (s *BroadcastingCheckpointsState) IsTerminal() bool {
	return false
}

// resolverStateSealed implements the sealed ResolverState interface.
func (s *BroadcastingCheckpointsState) resolverStateSealed() {}

// ResolvedState is a terminal state indicating the VTXO has been
// successfully unrolled onto the Bitcoin blockchain. All tree levels and
// checkpoint transactions (if applicable) have confirmed.
type ResolvedState struct {
	// Outpoint is the VTXO outpoint that was resolved.
	Outpoint wire.OutPoint

	// FinalOutpoint is the on-chain outpoint where the VTXO value can
	// be claimed.
	FinalOutpoint wire.OutPoint
}

// String returns a human-readable state name.
func (s *ResolvedState) String() string {
	return "Resolved"
}

// IsTerminal returns true since ResolvedState is terminal.
func (s *ResolvedState) IsTerminal() bool {
	return true
}

// resolverStateSealed implements the sealed ResolverState interface.
func (s *ResolvedState) resolverStateSealed() {}

// FailedState is a terminal state indicating an unrecoverable error
// occurred during the resolution process.
type FailedState struct {
	// Outpoint is the VTXO outpoint that failed to resolve.
	Outpoint wire.OutPoint

	// Reason describes what went wrong.
	Reason string

	// Err is the underlying error, if any.
	Err error
}

// String returns a human-readable state name.
func (s *FailedState) String() string {
	return fmt.Sprintf("Failed: %s", s.Reason)
}

// IsTerminal returns true since FailedState is terminal.
func (s *FailedState) IsTerminal() bool {
	return true
}

// resolverStateSealed implements the sealed ResolverState interface.
func (s *FailedState) resolverStateSealed() {}
