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
	operatorMBID := s.operatorMailboxID
	if operatorMBID == "" {
		return fmt.Errorf("server not ready: operator " +
			"mailbox ID not initialized")
	}

	// Derive the compound mailbox ID that the client addresses
	// envelopes to: operator:client. This gives each client a
	// unique server-side mailbox for Pull/checkpoint isolation.
	clientMBID := serverconn.PubKeyMailboxID(senderPubKey)
	compoundMBID := serverconn.CompoundMailboxID(
		operatorMBID, clientMBID,
	)

	// Verify the envelope is addressed to this client's
	// compound mailbox on this server.
	if env.Recipient != compoundMBID {
		return fmt.Errorf("envelope recipient %q does not "+
			"match expected mailbox %q",
			env.Recipient, compoundMBID)
	}

	// Verify the Schnorr auth signature from the envelope
	// headers. This proves the client holds the secp256k1
	// private key for their claimed mailbox identity. The
	// signature is bound to the compound mailbox ID.
	authSig := env.Headers[serverconn.AuthHeaderKey]
	if authSig == "" {
		return fmt.Errorf("missing %s header from client %q",
			serverconn.AuthHeaderKey, env.Sender)
	}

	if err := serverconn.VerifyMailboxAuth(
		senderPubKey, compoundMBID, authSig,
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

	// The compound mailbox ID is unique per client, satisfying
	// the bridge's uniqueness constraint while matching the
	// wire-level address the client sends to.
	cfg.LocalMailboxID = compoundMBID
	cfg.RemoteMailboxID = clientMBID
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
		"local_mailbox", compoundMBID,
		"remote_mailbox", env.Sender)

	return nil
}

// Compile-time interface check.
var _ clientconn.UnknownClientHandler = (*Server)(nil)
