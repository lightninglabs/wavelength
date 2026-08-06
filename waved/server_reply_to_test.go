package waved

import (
	"context"
	"testing"

	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
)

// recordingReplyEdge captures the envelopes handleInboundRPC sends so a test
// can assert where a response was addressed.
type recordingReplyEdge struct {
	sent []*mailboxpb.Envelope
}

// Send records the outbound envelope and reports success.
func (r *recordingReplyEdge) Send(_ context.Context, in *mailboxpb.SendRequest,
	_ ...grpc.CallOption) (*mailboxpb.SendResponse, error) {

	r.sent = append(r.sent, in.Envelope)

	return &mailboxpb.SendResponse{
		Status: &mailboxpb.Status{
			Ok: true,
		},
	}, nil
}

// Pull is unused by these tests.
func (r *recordingReplyEdge) Pull(_ context.Context, _ *mailboxpb.PullRequest,
	_ ...grpc.CallOption) (*mailboxpb.PullResponse, error) {

	return &mailboxpb.PullResponse{
		Status: &mailboxpb.Status{
			Ok: true,
		},
	}, nil
}

// AckUpTo is unused by these tests.
func (r *recordingReplyEdge) AckUpTo(_ context.Context,
	_ *mailboxpb.AckUpToRequest, _ ...grpc.CallOption) (
	*mailboxpb.AckUpToResponse, error) {

	return &mailboxpb.AckUpToResponse{
		Status: &mailboxpb.Status{
			Ok: true,
		},
	}, nil
}

// TestHandleInboundRPCAnswersTheSender asserts an inbound request is answered
// to the mailbox it came from, whatever its Rpc.ReplyTo says.
//
// The method dispatched here is deliberately unregistered, so ServeRPC fails
// and the handler takes its error path. That is the interesting path to pin:
// it still sends a response envelope, so it still has to address one.
func TestHandleInboundRPCAnswersTheSender(t *testing.T) {
	t.Parallel()

	const (
		operatorMailboxID = "operator-1"
		otherMailboxID    = "somebody-else"
	)

	tests := []struct {
		name    string
		replyTo string
	}{{
		name:    "matching reply-to",
		replyTo: operatorMailboxID,
	}, {
		// Previously produced an empty Recipient, which the mailbox
		// store rejects, so the caller never got its answer.
		name:    "absent reply-to",
		replyTo: "",
	}, {
		name:    "reply-to naming another mailbox",
		replyTo: otherMailboxID,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			edge := &recordingReplyEdge{}
			s := newCompatTestServer(t, edge)
			s.mailboxMux = mailboxrpc.NewServeMux()

			env := &mailboxpb.Envelope{
				Sender: operatorMailboxID,
				Body:   &anypb.Any{},
				Rpc: &mailboxpb.RpcMeta{
					Kind: mailboxpb.
						RpcMeta_KIND_REQUEST,
					Service:       "svc.Unregistered",
					Method:        "Method",
					CorrelationId: "corr-1",
					ReplyTo:       tc.replyTo,
				},
			}

			err := s.handleInboundRPC(t.Context(), edge, env)
			require.NoError(t, err)

			require.Len(t, edge.sent, 1)
			require.Equal(
				t, operatorMailboxID, edge.sent[0].Recipient,
			)
			require.NotEqual(
				t, otherMailboxID, edge.sent[0].Recipient,
			)
		})
	}
}
