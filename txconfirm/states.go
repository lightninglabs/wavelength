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
// progress.
type trackedTxStateBroadcasting struct {
	trackedTxData
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
				trackedTxData: s.trackedTxData,
				ConfirmHeight: e.BlockHeight,
			},
		}, nil

	case *trackedTxFailed:
		return &trackedTxStateTransition{
			NextState: &trackedTxStateFailed{
				trackedTxData: s.trackedTxData,
				Reason:        e.Reason,
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

// trackedTxStateConfirmed is the terminal confirmed state.
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

// IsTerminal returns true because confirmed is terminal.
func (s *trackedTxStateConfirmed) IsTerminal() bool {
	return true
}

// trackedTxStateSealed marks trackedTxStateConfirmed as a tracked-tx state.
func (s *trackedTxStateConfirmed) trackedTxStateSealed() {}

// ProcessEvent rejects unexpected events in the terminal confirmed state.
func (s *trackedTxStateConfirmed) ProcessEvent(_ context.Context,
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
