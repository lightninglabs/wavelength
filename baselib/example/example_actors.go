package baselib_test

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// ============================================================================
// Actor Services
// ============================================================================

// ReviewService handles document review requests.

// ReviewServiceKey is the service key for the review service.
var ReviewServiceKey = actor.NewServiceKey[
	ReviewMsg,
	ReviewResp](
	"review-service",
)

// ReviewMsg requests document review.
type ReviewMsg struct {
	actor.BaseMessage
	DocumentID string
	Author     string
	ReplyTo    actor.TellOnlyRef[protofsm.ActorMessage[DocEvent]]
}

// MessageType returns the message type.
func (m ReviewMsg) MessageType() string {
	return "ReviewRequest"
}

// ReviewResp is the response from review service.
type ReviewResp struct {
	Success bool
}

// ReviewServiceBehavior simulates async review process.
type ReviewServiceBehavior struct {
	// startProcessing gates review processing in the example so its
	// section header is always printed before async actor output.
	startProcessing <-chan struct{}
}

// Receive processes review requests.
func (r *ReviewServiceBehavior) Receive(ctx context.Context,
	msg ReviewMsg) fn.Result[ReviewResp] {

	if r.startProcessing != nil {
		select {
		case <-r.startProcessing:
		case <-ctx.Done():
			return fn.Err[ReviewResp](ctx.Err())
		}
	}

	fmt.Printf("ReviewService: Processing document %s by %s\n",
		msg.DocumentID, msg.Author)

	// Send confirmation that review started.
	msg.ReplyTo.Tell(ctx, protofsm.ActorMessage[DocEvent]{
		Event: EventReviewStarted{},
	})

	// Simulate review decision (approve in this example).
	msg.ReplyTo.Tell(ctx, protofsm.ActorMessage[DocEvent]{
		Event: EventApproved{
			Reviewer: "ReviewBot",
		},
	})

	return fn.Ok(ReviewResp{Success: true})
}

// NotificationService handles notification delivery.

// NotifyServiceKey is the service key for the notification service.
var NotifyServiceKey = actor.NewServiceKey[
	NotifyMsg,
	NotifyResp](
	"notify-service",
)

// NotifyMsg requests notification delivery.
type NotifyMsg struct {
	actor.BaseMessage
	Message string
}

// MessageType returns the message type.
func (m NotifyMsg) MessageType() string {
	return "Notify"
}

// NotifyResp is the response from notification service.
type NotifyResp struct {
	Success bool
}

// NotificationServiceBehavior delivers notifications.
type NotificationServiceBehavior struct{}

// Receive processes notification requests.
func (n *NotificationServiceBehavior) Receive(ctx context.Context,
	msg NotifyMsg) fn.Result[NotifyResp] {

	fmt.Printf("NotificationService: %s\n", msg.Message)

	return fn.Ok(NotifyResp{Success: true})
}
