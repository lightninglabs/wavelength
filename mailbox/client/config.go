package mailboxclient

import (
	"time"

	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
)

// Config holds configuration for a mailboxclient.Client.
type Config struct {
	// Edge is the gRPC client for the mailbox edge service.
	Edge mailboxpb.MailboxServiceClient

	// Store persists response payloads and pull cursor state.
	//
	// If unset, the client uses an in-memory store (not crash-safe).
	Store Store

	// LocalMailboxID is the mailbox id used to receive responses.
	LocalMailboxID string

	// RemoteMailboxID is the mailbox id used as the recipient for outbound
	// requests (typically the operator ingress mailbox).
	RemoteMailboxID string

	// ProtocolVersion is the protocol version set on all outbound
	// envelopes.
	ProtocolVersion uint32

	// PullMaxEnvelopes bounds the size of Pull batches.
	PullMaxEnvelopes uint32

	// PullWaitTimeout controls long-poll behavior.
	PullWaitTimeout time.Duration
}

// DefaultConfig returns a Config populated with conservative defaults.
func DefaultConfig() Config {
	return Config{
		PullMaxEnvelopes: 50,
		PullWaitTimeout:  5 * time.Second,
	}
}
