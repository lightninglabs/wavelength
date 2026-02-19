package chainresolver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/db"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// ProcessEvent handles events in WatchingCommitmentState. The resolver
// is waiting for a spend on the batch outpoint (fraud-reactive path).
func (s *WatchingCommitmentState) ProcessEvent(
	_ context.Context, event ResolverEvent,
	env *ResolverEnvironment) (*ResolverStateTransition, error) {

	switch evt := event.(type) {
	case *SpendDetectedResolverEvent:
		return s.handleSpendDetected(evt, env)

	case *ResolverFailedEvent:
		return &ResolverStateTransition{
			NextState: &FailedState{
				Outpoint: s.Outpoint,
				Reason:   evt.Reason,
				Err:      evt.Err,
			},
			NewEvents: fn.Some(ResolverEmittedEvent{
				Outbox: completionOutbox(
					s.Outpoint, wire.OutPoint{},
					false, evt.Reason,
				),
			}),
		}, nil

	default:
		return nil, fmt.Errorf(
			"watching_commitment: unexpected event: %T", event,
		)
	}
}

// handleSpendDetected inspects the spending transaction to determine which
// tree level is already on-chain and transitions to BroadcastingTreeState
// with that level pre-confirmed.
func (s *WatchingCommitmentState) handleSpendDetected(
	evt *SpendDetectedResolverEvent,
	env *ResolverEnvironment) (*ResolverStateTransition, error) {

	treePath := env.Ctx.TreePath
	if treePath == nil {
		return nil, fmt.Errorf("tree path is nil")
	}

	// The spending tx is the first level of the tree that was broadcast
	// by the counterparty. We start broadcasting from level 1 (since
	// level 0 is already on-chain).
	maxLevel := treePath.Depth() - 1

	nextState := &BroadcastingTreeState{
		Outpoint:            s.Outpoint,
		Trigger:             s.Trigger,
		CurrentLevel:        1,
		MaxLevel:            maxLevel,
		ConfirmedLevels:     1,
		AlreadyOnChainLevel: 0,
	}

	// Persist the state transition and begin broadcasting from the next
	// level.
	details, err := json.Marshal(nextState)
	if err != nil {
		return nil, fmt.Errorf("marshal state details: %w", err)
	}

	outbox := []ResolverOutMsg{
		&ResolverStatusUpdateOutMsg{
			Outpoint:     s.Outpoint,
			StateName:    "broadcasting_tree",
			StateDetails: details,
		},
	}

	// Emit broadcast messages for the next level's transactions.
	broadcastMsgs, err := buildTreeLevelBroadcasts(
		treePath, 1, s.Outpoint,
	)
	if err != nil {
		return nil, fmt.Errorf("build level broadcasts: %w", err)
	}

	outbox = append(outbox, broadcastMsgs...)

	return &ResolverStateTransition{
		NextState: nextState,
		NewEvents: fn.Some(ResolverEmittedEvent{Outbox: outbox}),
	}, nil
}

// ProcessEvent handles events in BroadcastingTreeState. The resolver
// broadcasts tree transactions level by level, waiting for confirmation
// at each level before advancing.
func (s *BroadcastingTreeState) ProcessEvent(
	_ context.Context, event ResolverEvent,
	env *ResolverEnvironment) (*ResolverStateTransition, error) {

	switch evt := event.(type) {
	case *StartResolveEvent:
		return s.handleStartResolve(evt, env)

	case *TreeLevelConfirmedEvent:
		return s.handleTreeLevelConfirmed(evt, env)

	case *ResolverFailedEvent:
		return &ResolverStateTransition{
			NextState: &FailedState{
				Outpoint: s.Outpoint,
				Reason:   evt.Reason,
				Err:      evt.Err,
			},
			NewEvents: fn.Some(ResolverEmittedEvent{
				Outbox: completionOutbox(
					s.Outpoint, wire.OutPoint{},
					false, evt.Reason,
				),
			}),
		}, nil

	default:
		return nil, fmt.Errorf(
			"broadcasting_tree: unexpected event: %T", event,
		)
	}
}

// handleStartResolve begins broadcasting the first tree level. This is the
// entry point for expiry and user-initiated triggers where the entire tree
// needs to be broadcast from the root.
func (s *BroadcastingTreeState) handleStartResolve(
	evt *StartResolveEvent,
	env *ResolverEnvironment) (*ResolverStateTransition, error) {

	treePath := env.Ctx.TreePath
	if treePath == nil {
		return nil, fmt.Errorf("tree path is nil")
	}

	// Build broadcast messages for the first level (root).
	broadcastMsgs, err := buildTreeLevelBroadcasts(
		treePath, s.CurrentLevel, s.Outpoint,
	)
	if err != nil {
		return nil, fmt.Errorf("build level broadcasts: %w", err)
	}

	// Persist the initial state.
	details, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("marshal state details: %w", err)
	}

	outbox := []ResolverOutMsg{
		&ResolverStatusUpdateOutMsg{
			Outpoint:     s.Outpoint,
			StateName:    "broadcasting_tree",
			StateDetails: details,
		},
	}
	outbox = append(outbox, broadcastMsgs...)

	return &ResolverStateTransition{
		NextState: s,
		NewEvents: fn.Some(ResolverEmittedEvent{Outbox: outbox}),
	}, nil
}

// handleTreeLevelConfirmed processes a confirmation at the current tree level
// and either advances to the next level, transitions to checkpoint
// broadcasting, or marks the resolution as complete.
func (s *BroadcastingTreeState) handleTreeLevelConfirmed(
	evt *TreeLevelConfirmedEvent,
	env *ResolverEnvironment) (*ResolverStateTransition, error) {

	newConfirmed := s.ConfirmedLevels + 1

	// Check if all tree levels are confirmed.
	allLevelsConfirmed := newConfirmed >= s.MaxLevel+1

	if allLevelsConfirmed {
		// Check if there are OOR packages to broadcast.
		oorPkgs := env.Ctx.OORPackages
		if oorPkgs != nil && len(oorPkgs.Packages) > 0 {
			return s.transitionToCheckpoints(
				evt, env, oorPkgs,
			)
		}

		// No OOR packages; resolution is complete. Compute the
		// final outpoint from the leaf node.
		finalOutpoint, err := computeLeafOutpoint(env.Ctx.TreePath)
		if err != nil {
			return nil, fmt.Errorf(
				"compute leaf outpoint: %w", err,
			)
		}

		return &ResolverStateTransition{
			NextState: &ResolvedState{
				Outpoint:      s.Outpoint,
				FinalOutpoint: finalOutpoint,
			},
			NewEvents: fn.Some(ResolverEmittedEvent{
				Outbox: completionOutbox(
					s.Outpoint, finalOutpoint,
					true, "tree broadcast complete",
				),
			}),
		}, nil
	}

	// Advance to the next tree level.
	nextLevel := s.CurrentLevel + 1
	nextState := &BroadcastingTreeState{
		Outpoint:            s.Outpoint,
		Trigger:             s.Trigger,
		CurrentLevel:        nextLevel,
		MaxLevel:            s.MaxLevel,
		ConfirmedLevels:     newConfirmed,
		AlreadyOnChainLevel: s.AlreadyOnChainLevel,
	}

	// Persist and broadcast the next level.
	details, err := json.Marshal(nextState)
	if err != nil {
		return nil, fmt.Errorf("marshal state details: %w", err)
	}

	outbox := []ResolverOutMsg{
		&ResolverStatusUpdateOutMsg{
			Outpoint:     s.Outpoint,
			StateName:    "broadcasting_tree",
			StateDetails: details,
		},
	}

	// Build broadcast messages for the next level.
	broadcastMsgs, err := buildTreeLevelBroadcasts(
		env.Ctx.TreePath, nextLevel, s.Outpoint,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"build level %d broadcasts: %w", nextLevel, err,
		)
	}

	outbox = append(outbox, broadcastMsgs...)

	return &ResolverStateTransition{
		NextState: nextState,
		NewEvents: fn.Some(ResolverEmittedEvent{Outbox: outbox}),
	}, nil
}

// transitionToCheckpoints builds the checkpoint broadcasting state and emits
// the initial outbox messages for the first checkpoint.
func (s *BroadcastingTreeState) transitionToCheckpoints(
	evt *TreeLevelConfirmedEvent,
	env *ResolverEnvironment,
	oorPkgs *db.OORUnrollPackages) (*ResolverStateTransition, error) {

	csvDelay := env.Ctx.VTXO.RelativeExpiry

	nextState := &BroadcastingCheckpointsState{
		Outpoint:          s.Outpoint,
		Packages:          oorPkgs.Packages,
		CurrentPackageIdx: 0,
		CSVDelay:          csvDelay,
		LastConfHeight:    -1,
		WaitingForCSV:     false,
	}

	details, err := json.Marshal(nextState)
	if err != nil {
		return nil, fmt.Errorf("marshal state details: %w", err)
	}

	outbox := []ResolverOutMsg{
		&ResolverStatusUpdateOutMsg{
			Outpoint:     s.Outpoint,
			StateName:    "broadcasting_checkpoints",
			StateDetails: details,
		},
	}

	// Build broadcast messages for the first checkpoint package.
	checkpointMsgs, err := buildCheckpointBroadcasts(
		oorPkgs.Packages[0], s.Outpoint,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"build checkpoint broadcasts: %w", err,
		)
	}

	outbox = append(outbox, checkpointMsgs...)

	return &ResolverStateTransition{
		NextState: nextState,
		NewEvents: fn.Some(ResolverEmittedEvent{Outbox: outbox}),
	}, nil
}

// ProcessEvent handles events in BroadcastingCheckpointsState. The resolver
// broadcasts checkpoint transactions, waiting for CSV delays between each.
func (s *BroadcastingCheckpointsState) ProcessEvent(
	_ context.Context, event ResolverEvent,
	env *ResolverEnvironment) (*ResolverStateTransition, error) {

	switch evt := event.(type) {
	case *CheckpointConfirmedEvent:
		return s.handleCheckpointConfirmed(evt, env)

	case *CSVMaturedEvent:
		return s.handleCSVMatured(evt, env)

	case *ResolverFailedEvent:
		return &ResolverStateTransition{
			NextState: &FailedState{
				Outpoint: s.Outpoint,
				Reason:   evt.Reason,
				Err:      evt.Err,
			},
			NewEvents: fn.Some(ResolverEmittedEvent{
				Outbox: completionOutbox(
					s.Outpoint, wire.OutPoint{},
					false, evt.Reason,
				),
			}),
		}, nil

	default:
		return nil, fmt.Errorf(
			"broadcasting_checkpoints: unexpected event: %T",
			event,
		)
	}
}

// handleCheckpointConfirmed records the confirmation height and begins the
// CSV wait period before the next checkpoint can be broadcast.
func (s *BroadcastingCheckpointsState) handleCheckpointConfirmed(
	evt *CheckpointConfirmedEvent,
	env *ResolverEnvironment) (*ResolverStateTransition, error) {

	nextPkgIdx := s.CurrentPackageIdx + 1

	// Check if all checkpoint packages have been broadcast and confirmed.
	if nextPkgIdx >= len(s.Packages) {
		// All checkpoints done; resolution is complete.
		finalOutpoint, err := computeLeafOutpoint(env.Ctx.TreePath)
		if err != nil {
			return nil, fmt.Errorf(
				"compute leaf outpoint: %w", err,
			)
		}

		return &ResolverStateTransition{
			NextState: &ResolvedState{
				Outpoint:      s.Outpoint,
				FinalOutpoint: finalOutpoint,
			},
			NewEvents: fn.Some(ResolverEmittedEvent{
				Outbox: completionOutbox(
					s.Outpoint, finalOutpoint,
					true,
					"checkpoints broadcast complete",
				),
			}),
		}, nil
	}

	// Enter CSV wait state before broadcasting the next checkpoint.
	nextState := &BroadcastingCheckpointsState{
		Outpoint:          s.Outpoint,
		Packages:          s.Packages,
		CurrentPackageIdx: nextPkgIdx,
		CSVDelay:          s.CSVDelay,
		LastConfHeight:    evt.BlockHeight,
		WaitingForCSV:     true,
	}

	details, err := json.Marshal(nextState)
	if err != nil {
		return nil, fmt.Errorf("marshal state details: %w", err)
	}

	return &ResolverStateTransition{
		NextState: nextState,
		NewEvents: fn.Some(ResolverEmittedEvent{
			Outbox: []ResolverOutMsg{
				&ResolverStatusUpdateOutMsg{
					Outpoint:     s.Outpoint,
					StateName:    "broadcasting_checkpoints",
					StateDetails: details,
				},
			},
		}),
	}, nil
}

// handleCSVMatured processes a CSV maturity event by broadcasting the next
// checkpoint package.
func (s *BroadcastingCheckpointsState) handleCSVMatured(
	_ *CSVMaturedEvent,
	env *ResolverEnvironment) (*ResolverStateTransition, error) {

	if s.CurrentPackageIdx >= len(s.Packages) {
		return nil, fmt.Errorf("package index out of bounds: %d",
			s.CurrentPackageIdx)
	}

	pkg := s.Packages[s.CurrentPackageIdx]

	// Build broadcast messages for this checkpoint package.
	broadcastMsgs, err := buildCheckpointBroadcasts(pkg, s.Outpoint)
	if err != nil {
		return nil, fmt.Errorf(
			"build checkpoint broadcasts: %w", err,
		)
	}

	// Transition out of CSV wait.
	nextState := &BroadcastingCheckpointsState{
		Outpoint:          s.Outpoint,
		Packages:          s.Packages,
		CurrentPackageIdx: s.CurrentPackageIdx,
		CSVDelay:          s.CSVDelay,
		LastConfHeight:    s.LastConfHeight,
		WaitingForCSV:     false,
	}

	details, err := json.Marshal(nextState)
	if err != nil {
		return nil, fmt.Errorf("marshal state details: %w", err)
	}

	outbox := []ResolverOutMsg{
		&ResolverStatusUpdateOutMsg{
			Outpoint:     s.Outpoint,
			StateName:    "broadcasting_checkpoints",
			StateDetails: details,
		},
	}
	outbox = append(outbox, broadcastMsgs...)

	return &ResolverStateTransition{
		NextState: nextState,
		NewEvents: fn.Some(ResolverEmittedEvent{Outbox: outbox}),
	}, nil
}

// ProcessEvent for ResolvedState. This is a terminal state, so all events
// result in staying in the same state.
func (s *ResolvedState) ProcessEvent(
	_ context.Context, _ ResolverEvent,
	_ *ResolverEnvironment) (*ResolverStateTransition, error) {

	// Terminal state: self-loop on all events.
	return &ResolverStateTransition{
		NextState: s,
	}, nil
}

// ProcessEvent for FailedState. This is a terminal state, so all events
// result in staying in the same state.
func (s *FailedState) ProcessEvent(
	_ context.Context, _ ResolverEvent,
	_ *ResolverEnvironment) (*ResolverStateTransition, error) {

	// Terminal state: self-loop on all events.
	return &ResolverStateTransition{
		NextState: s,
	}, nil
}

// completionOutbox builds the outbox messages emitted when a resolver reaches
// a terminal state (resolved or failed).
func completionOutbox(outpoint, finalOutpoint wire.OutPoint,
	success bool, reason string) []ResolverOutMsg {

	return []ResolverOutMsg{
		&ResolverStatusUpdateOutMsg{
			Outpoint:  outpoint,
			StateName: terminalStateName(success),
		},
		&ResolverCompletedOutMsg{
			Outpoint:      outpoint,
			FinalOutpoint: finalOutpoint,
			Success:       success,
			Reason:        reason,
		},
	}
}

// terminalStateName returns the persistence state name for the given terminal
// outcome.
func terminalStateName(success bool) string {
	if success {
		return "resolved"
	}

	return "failed"
}
