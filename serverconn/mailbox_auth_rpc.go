package serverconn

import (
	"context"
	"fmt"

	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// MailboxAuthSigner returns the hex-encoded x-mailbox-auth-sig value for a
// recipient mailbox ID.
type MailboxAuthSigner func(context.Context, string) (string, error)

// authenticatedMailboxClient adds mailbox auth metadata before forwarding to
// the configured mailbox transport.
type authenticatedMailboxClient struct {
	next mailboxpb.MailboxServiceClient
	sign MailboxAuthSigner
}

// NewAuthenticatedMailboxClient wraps a mailbox edge with per-mailbox auth
// metadata. Nil signers return next unchanged for unauthenticated test
// transports.
func NewAuthenticatedMailboxClient(next mailboxpb.MailboxServiceClient,
	sign MailboxAuthSigner) mailboxpb.MailboxServiceClient {

	if sign == nil {
		return next
	}

	return &authenticatedMailboxClient{
		next: next,
		sign: sign,
	}
}

// Send authenticates the sender for the envelope's recipient mailbox.
func (c *authenticatedMailboxClient) Send(ctx context.Context,
	req *mailboxpb.SendRequest, opts ...grpc.CallOption) (
	*mailboxpb.SendResponse, error) {

	recipient := ""
	if req != nil && req.Envelope != nil {
		recipient = req.Envelope.Recipient
	}

	ctx, err := c.authContext(ctx, recipient)
	if err != nil {
		return nil, err
	}

	return c.next.Send(ctx, req, opts...)
}

// Pull authenticates access to the requested mailbox.
func (c *authenticatedMailboxClient) Pull(ctx context.Context,
	req *mailboxpb.PullRequest, opts ...grpc.CallOption) (
	*mailboxpb.PullResponse, error) {

	recipient := ""
	if req != nil {
		recipient = req.MailboxId
	}

	ctx, err := c.authContext(ctx, recipient)
	if err != nil {
		return nil, err
	}

	return c.next.Pull(ctx, req, opts...)
}

// AckUpTo authenticates access to the requested mailbox cursor.
func (c *authenticatedMailboxClient) AckUpTo(ctx context.Context,
	req *mailboxpb.AckUpToRequest, opts ...grpc.CallOption) (
	*mailboxpb.AckUpToResponse, error) {

	recipient := ""
	if req != nil {
		recipient = req.MailboxId
	}

	ctx, err := c.authContext(ctx, recipient)
	if err != nil {
		return nil, err
	}

	return c.next.AckUpTo(ctx, req, opts...)
}

// authContext adds the mailbox auth metadata understood by both gRPC and the
// REST client's outgoing metadata bridge.
func (c *authenticatedMailboxClient) authContext(ctx context.Context,
	recipient string) (context.Context, error) {

	if recipient == "" {
		return nil, fmt.Errorf("mailbox recipient is required")
	}

	sig, err := c.sign(ctx, recipient)
	if err != nil {
		return nil, fmt.Errorf("sign mailbox auth: %w", err)
	}

	md, ok := metadata.FromOutgoingContext(ctx)
	if ok {
		md = md.Copy()
	} else {
		md = metadata.New(nil)
	}
	md.Set(AuthHeaderKey, sig)

	return metadata.NewOutgoingContext(ctx, md), nil
}
