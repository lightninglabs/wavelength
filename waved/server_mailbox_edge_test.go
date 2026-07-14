package waved

import (
	"context"
	"testing"

	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type stubMailboxServiceClient struct{}

func stubMailboxEdgeFactory(
	client mailboxpb.MailboxServiceClient,
) MailboxEdgeFactory {

	return func(
		_ grpc.ClientConnInterface,
		_ mailboxpb.MailboxServiceClient,
	) mailboxpb.MailboxServiceClient {

		return client
	}
}

func (s *stubMailboxServiceClient) Send(context.Context, *mailboxpb.SendRequest,
	...grpc.CallOption) (*mailboxpb.SendResponse, error) {

	return &mailboxpb.SendResponse{}, nil
}

func (s *stubMailboxServiceClient) Pull(context.Context, *mailboxpb.PullRequest,
	...grpc.CallOption) (*mailboxpb.PullResponse, error) {

	return &mailboxpb.PullResponse{}, nil
}

func (s *stubMailboxServiceClient) AckUpTo(context.Context,
	*mailboxpb.AckUpToRequest, ...grpc.CallOption) (
	*mailboxpb.AckUpToResponse, error) {

	return &mailboxpb.AckUpToResponse{}, nil
}

// TestServerNewMailboxEdgeUsesFactory verifies the daemon wiring can replace
// the default mailbox edge client for test harness interception.
func TestServerNewMailboxEdgeUsesFactory(t *testing.T) {
	t.Parallel()

	expected := &stubMailboxServiceClient{}
	server := &Server{
		cfg: &Config{
			MailboxEdgeFactory: stubMailboxEdgeFactory(expected),
		},
	}

	require.Same(t, expected, server.newMailboxEdge())
}
