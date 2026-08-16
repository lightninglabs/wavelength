package waved

import (
	"context"
	"testing"

	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
)

// recordingReplyEdge captures the envelopes handleInboundRPC sends so a test
// can assert where a response was addressed. When sendStatus is set it is
// returned verbatim, which lets a test drive the application-level rejection
// path that arrives as a non-OK status rather than as a transport error.
type recordingReplyEdge struct {
	sent []*mailboxpb.Envelope

	sendStatus *mailboxpb.Status
}

// Send records the outbound envelope and reports the configured status,
// defaulting to success.
func (r *recordingReplyEdge) Send(_ context.Context, in *mailboxpb.SendRequest,
	_ ...grpc.CallOption) (*mailboxpb.SendResponse, error) {

	r.sent = append(r.sent, in.Envelope)

	status := r.sendStatus
	if status == nil {
		status = &mailboxpb.Status{
			Ok: true,
		}
	}

	return &mailboxpb.SendResponse{
		Status: status,
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
		daemonMailboxID   = "this-daemon"
	)

	tests := []struct {
		name    string
		sender  string
		replyTo string
		wantErr string
	}{{
		name:    "matching reply-to",
		sender:  operatorMailboxID,
		replyTo: operatorMailboxID,
	}, {
		// Previously produced an empty Recipient, which the mailbox
		// store rejects, so the caller never got its answer.
		name:    "absent reply-to",
		sender:  operatorMailboxID,
		replyTo: "",
	}, {
		name:    "reply-to naming another mailbox",
		sender:  operatorMailboxID,
		replyTo: otherMailboxID,
	}, {
		// Sender is now the sole input deciding where the answer goes,
		// so an empty one reproduces the very failure the ReplyTo fix
		// removed. It must be refused before anything is sent.
		name:    "absent sender",
		sender:  "",
		replyTo: operatorMailboxID,
		wantErr: "missing envelope sender",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			edge := &recordingReplyEdge{}
			s := newCompatTestServer(t, edge)
			s.mailboxMux = mailboxrpc.NewServeMux()
			s.localMailboxID = daemonMailboxID

			env := &mailboxpb.Envelope{
				Sender: tc.sender,
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

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)

				// A refused envelope must not put an
				// unaddressable response on the wire.
				require.Empty(t, edge.sent)

				return
			}

			require.NoError(t, err)

			require.Len(t, edge.sent, 1)
			require.Equal(
				t, operatorMailboxID, edge.sent[0].Recipient,
			)
			require.NotEqual(
				t, otherMailboxID, edge.sent[0].Recipient,
			)

			// The other half of the envelope: the response is from
			// us, whoever it is addressed to.
			require.Equal(
				t, daemonMailboxID, edge.sent[0].Sender,
			)
		})
	}
}

// TestHandleInboundRPCReportsSendRejection asserts a mailbox rejection that
// arrives as a non-OK SendResponse.Status is reported as an error rather than
// swallowed. That status is the canonical application-level failure channel
// for the mailbox edge, so discarding it would report a lost answer as a
// successful dispatch and leave the caller blocked until its own deadline.
func TestHandleInboundRPCReportsSendRejection(t *testing.T) {
	t.Parallel()

	const rejectCode = "UNKNOWN_RECIPIENT"

	edge := &recordingReplyEdge{
		sendStatus: &mailboxpb.Status{
			Ok:      false,
			Code:    rejectCode,
			Message: "no such mailbox",
		},
	}

	s := newCompatTestServer(t, edge)
	s.mailboxMux = mailboxrpc.NewServeMux()
	s.localMailboxID = "this-daemon"

	env := &mailboxpb.Envelope{
		Sender: "operator-1",
		Body:   &anypb.Any{},
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_REQUEST,
			Service:       "svc.Unregistered",
			Method:        "Method",
			CorrelationId: "corr-1",
			ReplyTo:       "operator-1",
		},
	}

	err := s.handleInboundRPC(t.Context(), edge, env)
	require.Error(t, err)

	// The structured status must survive, not be flattened into a string,
	// so callers can classify a permanent version failure.
	var statusErr *mailboxconn.StatusError
	require.ErrorAs(t, err, &statusErr)
	require.Equal(t, rejectCode, statusErr.Code())

	// The send was attempted; only its outcome was misreported before.
	require.Len(t, edge.sent, 1)
}
