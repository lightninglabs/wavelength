package darepo

import (
	"context"
	"fmt"

	"github.com/btcsuite/btclog/v2"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
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
		}
	}

	return m.MailboxServiceServer.Send(ctx, req)
}

// HandleUnknownClient implements clientconn.UnknownClientHandler.
// It builds a PerClientConfig from the triggering envelope and
// registers the client with merged dispatchers from all active
// subsystems.
func (s *Server) HandleUnknownClient(ctx context.Context,
	clientID clientconn.ClientID,
	env *mailboxpb.Envelope) error {

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

	// The server pulls from the envelope's recipient mailbox
	// (where the client sends to) and sends responses back to
	// the client's sender mailbox (where the client pulls from).
	cfg.LocalMailboxID = env.Recipient
	cfg.RemoteMailboxID = env.Sender
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
		"local_mailbox", env.Recipient,
		"remote_mailbox", env.Sender)

	return nil
}

// Compile-time interface check.
var _ clientconn.UnknownClientHandler = (*Server)(nil)
