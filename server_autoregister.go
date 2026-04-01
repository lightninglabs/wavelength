package darepo

import (
	"context"
	"fmt"

	"github.com/btcsuite/btclog/v2"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo/clientconn"
)

// autoRegisteringMailbox wraps a MailboxServiceServer and calls the
// bridge's HandleInbound before each Send. This detects unknown
// clients at the transport boundary and triggers registration via
// the bridge's UnknownClientHandler, while keeping the underlying
// mailbox server free of side-effect hooks.
type autoRegisteringMailbox struct {
	mailboxpb.MailboxServiceServer

	bridge *clientconn.ClientsConnBridge
	log    btclog.Logger
}

// Send intercepts outbound envelopes to detect unknown clients
// before delegating to the underlying mailbox server. If the
// envelope's sender is unregistered, the bridge's unknown client
// handler fires and registers them.
func (m *autoRegisteringMailbox) Send(ctx context.Context,
	req *mailboxpb.SendRequest) (*mailboxpb.SendResponse, error) {

	if req != nil && req.Envelope != nil {
		if err := m.bridge.HandleInbound(
			ctx, req.Envelope,
		); err != nil {
			m.log.WarnS(ctx,
				"Auto-registration failed", err,
				"sender", req.Envelope.Sender,
			)

			return nil, fmt.Errorf("auto-registration "+
				"failed for %q: %w",
				req.Envelope.Sender, err)
		}
	}

	return m.MailboxServiceServer.Send(ctx, req)
}

// HandleUnknownClient implements clientconn.UnknownClientHandler.
// It builds a PerClientConfig from the triggering envelope and
// registers the client with merged dispatchers from all active
// subsystems. The client's mailbox ID must be its hex-encoded
// compressed public key, and the envelope must carry a valid
// Schnorr auth signature proving key ownership. Per-RPC access
// control (Pull/AckUpTo) is enforced separately by the mTLS
// interceptor.
func (s *Server) HandleUnknownClient(ctx context.Context,
	clientID clientconn.ClientID,
	env *mailboxpb.Envelope) error {

	// Verify the client's claimed identity. The sender field
	// must be a hex-encoded compressed pubkey.
	senderPubKey, err := serverconn.ParseMailboxPubKey(
		env.Sender,
	)
	if err != nil {
		return fmt.Errorf("invalid sender mailbox ID %q: %w",
			env.Sender, err)
	}

	// Use the cached operator mailbox ID derived at startup.
	// Guard against calls before rounds subsystem initializes
	// the operator key.
	serverMailboxID := s.operatorMailboxID
	if serverMailboxID == "" {
		return fmt.Errorf("server not ready: operator " +
			"mailbox ID not initialized")
	}

	// Verify the envelope is addressed to this server's
	// mailbox. Reject envelopes with mismatched recipients
	// before checking the auth signature.
	if env.Recipient != serverMailboxID {
		return fmt.Errorf("envelope recipient %q does not "+
			"match server mailbox %q",
			env.Recipient, serverMailboxID)
	}

	// Verify the Schnorr auth signature from the envelope
	// headers. This proves the client holds the secp256k1
	// private key for their claimed mailbox identity.
	authSig := env.Headers[serverconn.AuthHeaderKey]
	if authSig == "" {
		return fmt.Errorf("missing %s header from client %q",
			serverconn.AuthHeaderKey, env.Sender)
	}

	if err := serverconn.VerifyMailboxAuth(
		senderPubKey, serverMailboxID, authSig,
	); err != nil {
		return fmt.Errorf("auth verification failed for "+
			"client %q: %w", env.Sender, err)
	}

	// Build an in-process edge client for the new client's
	// runtime. Each runtime gets its own edge instance backed
	// by the shared mailbox store.
	edgeClient, err := NewLocalMailboxClient(s.mailboxStore)
	if err != nil {
		return fmt.Errorf("build edge for %q: %w",
			clientID, err)
	}

	cfg := clientconn.DefaultPerClientConfig()
	cfg.Edge = edgeClient

	// Derive a per-client unique local mailbox ID by combining
	// the operator mailbox with the client's identity. The
	// bridge requires unique LocalMailboxIDs across all clients
	// because checkpoints and durable actor state are keyed by
	// this value. The remote mailbox ID uses the canonical
	// form derived from the parsed public key.
	cfg.LocalMailboxID = serverMailboxID + ":" + env.Sender
	cfg.RemoteMailboxID = serverconn.PubKeyMailboxID(
		senderPubKey,
	)
	cfg.Store = s.deliveryStore
	cfg.ProtocolVersion = env.ProtocolVersion

	_, err = s.RegisterClientWithAllDispatchers(
		ctx, clientID, cfg,
	)
	if err != nil {
		return fmt.Errorf("auto-register client %q: %w",
			clientID, err)
	}

	s.log.InfoS(ctx, "Auto-registered external client",
		"client_id", string(clientID),
		"local_mailbox", serverMailboxID,
		"remote_mailbox", env.Sender)

	return nil
}

// Compile-time interface check.
var _ clientconn.UnknownClientHandler = (*Server)(nil)
