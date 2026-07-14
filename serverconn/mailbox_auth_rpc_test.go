package serverconn

import (
	"context"
	"testing"

	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func TestAuthenticatedMailboxClientAddsMetadata(t *testing.T) {
	t.Parallel()

	next := &capturingMailboxClient{}
	sign := func(_ context.Context, recipient string) (string, error) {
		return "auth-" + recipient, nil
	}
	client := NewAuthenticatedMailboxClient(
		next, sign,
	)

	_, err := client.Pull(t.Context(), &mailboxpb.PullRequest{
		MailboxId: "mailbox",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"auth-mailbox"}, next.authHeader)
}

func TestAuthenticatedMailboxClientReplacesMetadata(t *testing.T) {
	t.Parallel()

	next := &capturingMailboxClient{}
	sign := func(_ context.Context, recipient string) (string, error) {
		return "auth-" + recipient, nil
	}
	client := NewAuthenticatedMailboxClient(
		next, sign,
	)

	ctx := metadata.AppendToOutgoingContext(
		t.Context(), AuthHeaderKey, "caller-auth",
	)
	_, err := client.Pull(ctx, &mailboxpb.PullRequest{
		MailboxId: "mailbox",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"auth-mailbox"}, next.authHeader)
}

type capturingMailboxClient struct {
	authHeader []string
}

func (c *capturingMailboxClient) Send(ctx context.Context,
	_ *mailboxpb.SendRequest, _ ...grpc.CallOption) (
	*mailboxpb.SendResponse, error) {

	c.capture(ctx)

	return &mailboxpb.SendResponse{}, nil
}

func (c *capturingMailboxClient) Pull(ctx context.Context,
	_ *mailboxpb.PullRequest, _ ...grpc.CallOption) (
	*mailboxpb.PullResponse, error) {

	c.capture(ctx)

	return &mailboxpb.PullResponse{}, nil
}

func (c *capturingMailboxClient) AckUpTo(ctx context.Context,
	_ *mailboxpb.AckUpToRequest, _ ...grpc.CallOption) (
	*mailboxpb.AckUpToResponse, error) {

	c.capture(ctx)

	return &mailboxpb.AckUpToResponse{}, nil
}

func (c *capturingMailboxClient) capture(ctx context.Context) {
	md, _ := metadata.FromOutgoingContext(ctx)
	c.authHeader = md.Get(AuthHeaderKey)
}
