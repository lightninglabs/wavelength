package serverconn

import (
	"context"

	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"google.golang.org/grpc"
)

// stampEnvelopeVersions writes the immutable mailbox transport and Ark protocol
// version pair onto an envelope, overwriting any pre-existing values so no send
// path can rely on a caller-provided version. A nil envelope is a no-op. It is
// the single stamping implementation shared by the edge decorator and the
// exported response-path entry point.
func stampEnvelopeVersions(env *mailboxpb.Envelope, mailboxVersion,
	arkVersion uint32) {

	if env == nil {
		return
	}

	env.ProtocolVersion = mailboxVersion
	env.ArkProtocolVersion = arkVersion
}

// versionStampingMailboxClient is a MailboxServiceClient decorator that stamps
// the runtime-bound version pair onto every outbound envelope before forwarding
// to the wrapped edge. It centralizes stamping in one interceptor-style
// location so no individual send path has to remember to stamp; only Send
// carries an envelope, so Pull and AckUpTo forward unchanged. It mirrors the
// auth decorator (authenticatedMailboxClient) layered over the same edge.
type versionStampingMailboxClient struct {
	next           mailboxpb.MailboxServiceClient
	mailboxVersion uint32
	arkVersion     uint32
}

// newVersionStampingMailboxClient wraps next so every Send stamps the given
// immutable version pair. A nil next is returned unchanged so a connector built
// without an edge (rejected later by NewRuntime) does not gain a non-nil
// wrapper around a nil client.
func newVersionStampingMailboxClient(next mailboxpb.MailboxServiceClient,
	mailboxVersion, arkVersion uint32) mailboxpb.MailboxServiceClient {

	if next == nil {
		return nil
	}

	return &versionStampingMailboxClient{
		next:           next,
		mailboxVersion: mailboxVersion,
		arkVersion:     arkVersion,
	}
}

// Send stamps the envelope's version pair, then forwards to the wrapped edge.
func (c *versionStampingMailboxClient) Send(ctx context.Context,
	req *mailboxpb.SendRequest, opts ...grpc.CallOption) (
	*mailboxpb.SendResponse, error) {

	if req != nil {
		stampEnvelopeVersions(
			req.Envelope, c.mailboxVersion, c.arkVersion,
		)
	}

	return c.next.Send(ctx, req, opts...)
}

// Pull forwards unchanged: a pull request carries no envelope to stamp.
func (c *versionStampingMailboxClient) Pull(ctx context.Context,
	req *mailboxpb.PullRequest, opts ...grpc.CallOption) (
	*mailboxpb.PullResponse, error) {

	return c.next.Pull(ctx, req, opts...)
}

// AckUpTo forwards unchanged: an ack request carries no envelope to stamp.
func (c *versionStampingMailboxClient) AckUpTo(ctx context.Context,
	req *mailboxpb.AckUpToRequest, opts ...grpc.CallOption) (
	*mailboxpb.AckUpToResponse, error) {

	return c.next.AckUpTo(ctx, req, opts...)
}
