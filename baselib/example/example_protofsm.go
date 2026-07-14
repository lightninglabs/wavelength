package baselib_test

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// This file contains an example document approval workflow implementation
// that demonstrates how to integrate a state machine with the actor system.
//
// The workflow models: Init -> AwaitingReview -> Approved/Rejected

// ============================================================================
// Event Types (Sealed Interface)
// ============================================================================

// DocEvent represents all possible events in the document workflow FSM.
type DocEvent interface {
	isDocEventSealed()
}

// EventSubmitDocument is sent to start the approval workflow.
type EventSubmitDocument struct {
	DocumentID string
	Author     string
}

func (EventSubmitDocument) isDocEventSealed() {}

// EventReviewStarted is sent when the review service accepts the request.
type EventReviewStarted struct{}

func (EventReviewStarted) isDocEventSealed() {}

// EventApproved is sent when the document is approved.
type EventApproved struct {
	Reviewer string
}

func (EventApproved) isDocEventSealed() {}

// EventRejected is sent when the document is rejected.
type EventRejected struct {
	Reviewer string
	Reason   string
}

func (EventRejected) isDocEventSealed() {}

// EventResume is sent when resuming from storage.
type EventResume struct{}

func (EventResume) isDocEventSealed() {}

// ============================================================================
// Outbox Events (Routed to Actor Services)
// ============================================================================

// DocOutboxEvent is the sealed interface for events routed to actors.
type DocOutboxEvent interface {
	protofsm.ActorOutboxEvent

	isDocOutboxEventSealed()
}

// OutboxRequestReview requests document review from ReviewService.
type OutboxRequestReview struct {
	protofsm.RoutedOutboxEvent[ReviewMsg, ReviewResp]
}

// NewOutboxRequestReview creates a new review request outbox event.
func NewOutboxRequestReview(documentID string, author string,
	fsmRef actor.TellOnlyRef[protofsm.ActorMessage[DocEvent]]) OutboxRequestReview {

	return OutboxRequestReview{
		RoutedOutboxEvent: protofsm.NewTellOutboxEvent(
			ReviewServiceKey,
			ReviewMsg{
				DocumentID: documentID,
				Author:     author,
				ReplyTo:    fsmRef,
			},
		),
	}
}

func (OutboxRequestReview) isDocOutboxEventSealed() {}

// OutboxNotify sends notification via NotificationService.
type OutboxNotify struct {
	protofsm.RoutedOutboxEvent[NotifyMsg, NotifyResp]
}

// NewOutboxNotify creates a new notification outbox event.
func NewOutboxNotify(message string) OutboxNotify {
	return OutboxNotify{
		RoutedOutboxEvent: protofsm.NewTellOutboxEvent(
			NotifyServiceKey, NotifyMsg{
				Message: message,
			},
		),
	}
}

func (OutboxNotify) isDocOutboxEventSealed() {}

// ============================================================================
// State Types
// ============================================================================

// DocState represents all possible states in the document workflow FSM.
type DocState interface {
	protofsm.State[DocEvent, DocOutboxEvent, *DocEnvironment]

	isDocStateSealed()
}

// DocEnvironment holds the FSM execution context.
type DocEnvironment struct {
	actorRef actor.TellOnlyRef[protofsm.ActorMessage[DocEvent]]
}

// SetTellOnlyRef sets the actor reference for the environment.
func (e *DocEnvironment) SetTellOnlyRef(
	ref actor.TellOnlyRef[protofsm.ActorMessage[DocEvent]]) {

	e.actorRef = ref
}

// GetTellOnlyRef returns the actor reference from the environment.
func (e *DocEnvironment) GetTellOnlyRef() actor.TellOnlyRef[protofsm.ActorMessage[DocEvent]] {
	return e.actorRef
}

// Compile-time check for TellRefEnv.
var _ protofsm.TellRefEnv[DocEvent] = (*DocEnvironment)(nil)

// StateInit is the initial state.
type StateInit struct{}

func (StateInit) isDocStateSealed() {}

func (StateInit) IsTerminal() bool {
	return false
}

func (StateInit) String() string {
	return "Init"
}

// ProcessEvent processes events in the Init state.
func (s *StateInit) ProcessEvent(ctx context.Context, event DocEvent,
	env *DocEnvironment) (
	*protofsm.StateTransition[DocEvent, DocOutboxEvent, *DocEnvironment],
	error) {

	switch e := event.(type) {
	case EventSubmitDocument:
		fmt.Printf("Document %s submitted by %s\n", e.DocumentID,
			e.Author)

		nextState := &StateAwaitingReview{
			documentID: e.DocumentID,
			author:     e.Author,
		}

		return &protofsm.StateTransition[
			DocEvent,
			DocOutboxEvent,
			*DocEnvironment,
		]{
			NextState: nextState,
			NewEvents: fn.Some(protofsm.EmittedEvent[
				DocEvent,
				DocOutboxEvent,
			]{
				Outbox: []DocOutboxEvent{
					NewOutboxRequestReview(
						e.DocumentID, e.Author,
						env.actorRef,
					),
				},
			}),
		}, nil

	default:
		return nil, fmt.Errorf("unexpected event %T in StateInit", e)
	}
}

// StateAwaitingReview waits for review decision.
type StateAwaitingReview struct {
	documentID string
	author     string
}

func (StateAwaitingReview) isDocStateSealed() {}

func (StateAwaitingReview) IsTerminal() bool {
	return false
}

func (StateAwaitingReview) String() string {
	return "AwaitingReview"
}

// ProcessEvent processes events in the AwaitingReview state.
func (s *StateAwaitingReview) ProcessEvent(ctx context.Context, event DocEvent,
	env *DocEnvironment) (
	*protofsm.StateTransition[DocEvent, DocOutboxEvent, *DocEnvironment],
	error) {

	switch e := event.(type) {
	case EventResume:
		// Re-emit review request on resume.
		return &protofsm.StateTransition[
			DocEvent,
			DocOutboxEvent,
			*DocEnvironment,
		]{
			NextState: s,
			NewEvents: fn.Some(protofsm.EmittedEvent[
				DocEvent,
				DocOutboxEvent,
			]{
				Outbox: []DocOutboxEvent{
					NewOutboxRequestReview(
						s.documentID, s.author,
						env.actorRef,
					),
				},
			}),
		}, nil

	case EventReviewStarted:
		fmt.Printf("Review started for document %s\n", s.documentID)

		// Stay in same state, just acknowledge.
		return &protofsm.StateTransition[
			DocEvent,
			DocOutboxEvent,
			*DocEnvironment,
		]{
			NextState: s,
		}, nil

	case EventApproved:
		fmt.Printf("Document %s approved by %s\n", s.documentID,
			e.Reviewer)

		nextState := &StateApproved{}
		notifyMsg := fmt.Sprintf("Document %s approved by %s",
			s.documentID, e.Reviewer)

		return &protofsm.StateTransition[
			DocEvent,
			DocOutboxEvent,
			*DocEnvironment,
		]{
			NextState: nextState,
			NewEvents: fn.Some(protofsm.EmittedEvent[
				DocEvent,
				DocOutboxEvent,
			]{
				Outbox: []DocOutboxEvent{
					NewOutboxNotify(notifyMsg),
				},
			}),
		}, nil

	case EventRejected:
		fmt.Printf("Document %s rejected by %s: %s\n", s.documentID,
			e.Reviewer, e.Reason)

		nextState := &StateRejected{}
		notifyMsg := fmt.Sprintf("Document %s rejected by %s: %s",
			s.documentID, e.Reviewer, e.Reason)

		return &protofsm.StateTransition[
			DocEvent,
			DocOutboxEvent,
			*DocEnvironment,
		]{
			NextState: nextState,
			NewEvents: fn.Some(protofsm.EmittedEvent[
				DocEvent,
				DocOutboxEvent,
			]{
				Outbox: []DocOutboxEvent{
					NewOutboxNotify(notifyMsg),
				},
			}),
		}, nil

	default:
		return nil, fmt.Errorf("unexpected event %T in "+
			"StateAwaitingReview", e)
	}
}

// StateApproved is a terminal state.
type StateApproved struct{}

func (StateApproved) isDocStateSealed() {}

func (StateApproved) IsTerminal() bool {
	return true
}

func (StateApproved) String() string {
	return "Approved"
}

// ProcessEvent processes events in the Approved state.
func (s *StateApproved) ProcessEvent(ctx context.Context, event DocEvent,
	env *DocEnvironment) (
	*protofsm.StateTransition[DocEvent, DocOutboxEvent, *DocEnvironment],
	error) {

	return nil, fmt.Errorf("no events expected in terminal state")
}

// StateRejected is a terminal state.
type StateRejected struct{}

func (StateRejected) isDocStateSealed() {}

func (StateRejected) IsTerminal() bool {
	return true
}

func (StateRejected) String() string {
	return "Rejected"
}

// ProcessEvent processes events in the Rejected state.
func (s *StateRejected) ProcessEvent(ctx context.Context, event DocEvent,
	env *DocEnvironment) (
	*protofsm.StateTransition[DocEvent, DocOutboxEvent, *DocEnvironment],
	error) {

	return nil, fmt.Errorf("no events expected in terminal state")
}
