package txconfirm

import (
	"context"
	"fmt"
)

// trackedTxStateNew is the initial tracked-tx FSM state.
type trackedTxStateNew struct {
	trackedTxData
}

// String returns a human-readable representation of the initial state.
func (s *trackedTxStateNew) String() string {
	return "New"
}

// IsTerminal returns false because the initial state is not terminal.
func (s *trackedTxStateNew) IsTerminal() bool {
	return false
}

// trackedTxStateSealed marks trackedTxStateNew as a tracked-tx state.
func (s *trackedTxStateNew) trackedTxStateSealed() {}

// ProcessEvent applies one event to the initial tracked-tx state.
func (s *trackedTxStateNew) ProcessEvent(_ context.Context,
	event trackedTxEvent, _ *trackedTxEnvironment) (
	*trackedTxStateTransition, error) {

	switch event := event.(type) {
	case *trackedTxBroadcastStarted:
		return &trackedTxStateTransition{
			NextState: &trackedTxStateBroadcasting{
				trackedTxData: s.trackedTxData,
			},
		}, nil

	case *trackedTxFailed:
		return &trackedTxStateTransition{
			NextState: &trackedTxStateFailed{
				trackedTxData: s.trackedTxData,
				Reason:        event.Reason,
			},
		}, nil

	default:
		return nil, fmt.Errorf("unexpected event %T in %s", event, s)
	}
}

// trackedTxStateBroadcasting indicates the initial broadcast attempt is in
// progress, or has failed to reach any mempool and is awaiting re-attempt.
// The embedded progress carries the last attempt height and the consecutive
// failure counter so the actor can pace retries and escalate to the operator.
type trackedTxStateBroadcasting struct {
	trackedTxData
	trackedTxProgress
}

// String returns a human-readable representation of the broadcasting state.
func (s *trackedTxStateBroadcasting) String() string {
	return "Broadcasting"
}

// IsTerminal returns false because broadcasting is not terminal.
func (s *trackedTxStateBroadcasting) IsTerminal() bool {
	return false
}

// trackedTxStateSealed marks trackedTxStateBroadcasting as a tracked-tx state.
func (s *trackedTxStateBroadcasting) trackedTxStateSealed() {}

// ProcessEvent applies one event to the broadcasting state.
func (s *trackedTxStateBroadcasting) ProcessEvent(_ context.Context,
	event trackedTxEvent, _ *trackedTxEnvironment) (
	*trackedTxStateTransition, error) {

	switch e := event.(type) {
	case *trackedTxBroadcastStarted:
		// A re-attempt of the initial broadcast self-loops the
		// Broadcasting state, preserving the accumulated progress
		// (last attempt height and failure counter) until the attempt
		// resolves to acceptance or another failure.
		return &trackedTxStateTransition{
			NextState: &trackedTxStateBroadcasting{
				trackedTxData:     s.trackedTxData,
				trackedTxProgress: s.trackedTxProgress,
			},
		}, nil

	case *trackedTxBroadcastFailed:
		// The attempt reached no mempool. Stay in Broadcasting with
		// the updated progress so the next interval re-attempts rather
		// than falsely reporting AwaitingConfirmation.
		return &trackedTxStateTransition{
			NextState: &trackedTxStateBroadcasting{
				trackedTxData:     s.trackedTxData,
				trackedTxProgress: e.Progress,
			},
		}, nil

	case *trackedTxBroadcastAccepted:
		return &trackedTxStateTransition{
			NextState: &trackedTxStateAwaitingConfirmation{
				trackedTxData:     s.trackedTxData,
				trackedTxProgress: e.Progress,
			},
		}, nil

	case *trackedTxConfirmed:
		return &trackedTxStateTransition{
			NextState: &trackedTxStateConfirmed{
				trackedTxData:     s.trackedTxData,
				trackedTxProgress: s.trackedTxProgress,
				ConfirmHeight:     e.BlockHeight,
			},
		}, nil

	case *trackedTxFailed:
		return &trackedTxStateTransition{
			NextState: &trackedTxStateFailed{
				trackedTxData:     s.trackedTxData,
				trackedTxProgress: s.trackedTxProgress,
				Reason:            e.Reason,
			},
		}, nil

	default:
		return nil, fmt.Errorf("unexpected event %T in %s", event, s)
	}
}

// trackedTxStateAwaitingConfirmation indicates the parent transaction is
// waiting for the target confirmation count.
type trackedTxStateAwaitingConfirmation struct {
	trackedTxData
	trackedTxProgress
}

// String returns a human-readable representation of awaiting confirmation.
func (s *trackedTxStateAwaitingConfirmation) String() string {
	return "AwaitingConfirmation"
}

// IsTerminal returns false because awaiting confirmation is not terminal.
func (s *trackedTxStateAwaitingConfirmation) IsTerminal() bool {
	return false
}

// trackedTxStateSealed marks trackedTxStateAwaitingConfirmation as a tracked
// tx state.
func (s *trackedTxStateAwaitingConfirmation) trackedTxStateSealed() {}

// ProcessEvent applies one event to the awaiting-confirmation state.
func (s *trackedTxStateAwaitingConfirmation) ProcessEvent(_ context.Context,
	event trackedTxEvent, _ *trackedTxEnvironment) (
	*trackedTxStateTransition, error) {

	switch event := event.(type) {
	case *trackedTxFeeBumpStarted:
		return &trackedTxStateTransition{
			NextState: &trackedTxStateFeeBumping{
				trackedTxData:     s.trackedTxData,
				trackedTxProgress: s.trackedTxProgress,
			},
		}, nil

	case *trackedTxConfirmed:
		return &trackedTxStateTransition{
			NextState: &trackedTxStateConfirmed{
				trackedTxData:     s.trackedTxData,
				trackedTxProgress: s.trackedTxProgress,
				ConfirmHeight:     event.BlockHeight,
			},
		}, nil

	case *trackedTxFailed:
		return &trackedTxStateTransition{
			NextState: &trackedTxStateFailed{
				trackedTxData:     s.trackedTxData,
				trackedTxProgress: s.trackedTxProgress,
				Reason:            event.Reason,
			},
		}, nil

	default:
		return nil, fmt.Errorf("unexpected event %T in %s", event, s)
	}
}

// trackedTxStateFeeBumping indicates the tracked txid is currently attempting
// a CPFP fee bump.
type trackedTxStateFeeBumping struct {
	trackedTxData
	trackedTxProgress
}

// String returns a human-readable representation of the fee-bumping state.
func (s *trackedTxStateFeeBumping) String() string {
	return "FeeBumping"
}

// IsTerminal returns false because fee-bumping is not terminal.
func (s *trackedTxStateFeeBumping) IsTerminal() bool {
	return false
}

// trackedTxStateSealed marks trackedTxStateFeeBumping as a tracked-tx state.
func (s *trackedTxStateFeeBumping) trackedTxStateSealed() {}

// ProcessEvent applies one event to the fee-bumping state.
func (s *trackedTxStateFeeBumping) ProcessEvent(_ context.Context,
	event trackedTxEvent, _ *trackedTxEnvironment) (
	*trackedTxStateTransition, error) {

	switch e := event.(type) {
	case *trackedTxBroadcastAccepted:
		progress := e.Progress
		progress.BumpCount = s.BumpCount + 1

		return &trackedTxStateTransition{
			NextState: &trackedTxStateAwaitingConfirmation{
				trackedTxData:     s.trackedTxData,
				trackedTxProgress: progress,
			},
		}, nil

	case *trackedTxConfirmed:
		return &trackedTxStateTransition{
			NextState: &trackedTxStateConfirmed{
				trackedTxData:     s.trackedTxData,
				trackedTxProgress: s.trackedTxProgress,
				ConfirmHeight:     e.BlockHeight,
			},
		}, nil

	case *trackedTxFailed:
		return &trackedTxStateTransition{
			NextState: &trackedTxStateFailed{
				trackedTxData:     s.trackedTxData,
				trackedTxProgress: s.trackedTxProgress,
				Reason:            e.Reason,
			},
		}, nil

	default:
		return nil, fmt.Errorf("unexpected event %T in %s", event, s)
	}
}

// trackedTxStateConfirmed is the reorg-reversible confirmed state. A
// transaction in this state has reached its target confirmation count on
// the canonical chain; a subsequent reorg moves it back to
// AwaitingConfirmation, and finality moves it to Finalized.
type trackedTxStateConfirmed struct {
	trackedTxData
	trackedTxProgress

	// ConfirmHeight is the block height where the tx confirmed.
	ConfirmHeight int32
}

// String returns a human-readable representation of the confirmed state.
func (s *trackedTxStateConfirmed) String() string {
	return "Confirmed"
}

// IsTerminal returns false because the confirmation is reversible until
// the backend reports finality via trackedTxFinalized.
func (s *trackedTxStateConfirmed) IsTerminal() bool {
	return false
}

// trackedTxStateSealed marks trackedTxStateConfirmed as a tracked-tx state.
func (s *trackedTxStateConfirmed) trackedTxStateSealed() {}

// ProcessEvent applies one event to the confirmed state. A reorg moves
// the FSM back to AwaitingConfirmation; finality moves it to the terminal
// Finalized state.
func (s *trackedTxStateConfirmed) ProcessEvent(_ context.Context,
	event trackedTxEvent, _ *trackedTxEnvironment) (
	*trackedTxStateTransition, error) {

	switch event.(type) {
	case *trackedTxReorged:
		return &trackedTxStateTransition{
			NextState: &trackedTxStateAwaitingConfirmation{
				trackedTxData:     s.trackedTxData,
				trackedTxProgress: s.trackedTxProgress,
			},
		}, nil

	case *trackedTxFinalized:
		return &trackedTxStateTransition{
			NextState: &trackedTxStateFinalized{
				trackedTxData:     s.trackedTxData,
				trackedTxProgress: s.trackedTxProgress,
				ConfirmHeight:     s.ConfirmHeight,
			},
		}, nil

	default:
		return nil, fmt.Errorf("unexpected event %T in %s", event, s)
	}
}

// trackedTxStateFinalized is the terminal "confirmed past reorg safety
// depth" state. No further events are accepted.
type trackedTxStateFinalized struct {
	trackedTxData
	trackedTxProgress

	// ConfirmHeight is the block height where the tx confirmed before
	// being finalized.
	ConfirmHeight int32
}

// String returns a human-readable representation of the finalized state.
func (s *trackedTxStateFinalized) String() string {
	return "Finalized"
}

// IsTerminal returns true because finalized is terminal.
func (s *trackedTxStateFinalized) IsTerminal() bool {
	return true
}

// trackedTxStateSealed marks trackedTxStateFinalized as a tracked-tx state.
func (s *trackedTxStateFinalized) trackedTxStateSealed() {}

// ProcessEvent rejects unexpected events in the terminal finalized state.
func (s *trackedTxStateFinalized) ProcessEvent(_ context.Context,
	event trackedTxEvent, _ *trackedTxEnvironment) (
	*trackedTxStateTransition, error) {

	return nil, fmt.Errorf("unexpected event %T in %s", event, s)
}

// trackedTxStateFailed is the terminal failure state.
type trackedTxStateFailed struct {
	trackedTxData
	trackedTxProgress

	// Reason is the stable human-readable failure reason.
	Reason string
}

// String returns a human-readable representation of the failed state.
func (s *trackedTxStateFailed) String() string {
	return "Failed"
}

// IsTerminal returns true because failed is terminal.
func (s *trackedTxStateFailed) IsTerminal() bool {
	return true
}

// trackedTxStateSealed marks trackedTxStateFailed as a tracked-tx state.
func (s *trackedTxStateFailed) trackedTxStateSealed() {}

// ProcessEvent rejects unexpected events in the terminal failed state.
func (s *trackedTxStateFailed) ProcessEvent(_ context.Context,
	event trackedTxEvent, _ *trackedTxEnvironment) (
	*trackedTxStateTransition, error) {

	return nil, fmt.Errorf("unexpected event %T in %s", event, s)
}
