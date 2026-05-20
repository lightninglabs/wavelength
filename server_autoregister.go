package darepo

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
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

			return nil, fmt.Errorf("auto-registration failed for "+
				"%q: %w", req.Envelope.Sender, err)
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
	clientID clientconn.ClientID, env *mailboxpb.Envelope) error {

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
		return fmt.Errorf("server not ready: operator mailbox ID not " +
			"initialized")
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
		return fmt.Errorf("envelope recipient %q does not match "+
			"expected mailbox %q", env.Recipient, compoundMBID)
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
		return fmt.Errorf("auth verification failed for client %q: %w",
			env.Sender, err)
	}

	// Schnorr ownership of the secp256k1 identity is now proven.
	// Before binding the TLS leaf fingerprint, also verify that
	// the secp256k1 holder signed over the SPKI of the TLS leaf
	// we actually observed on this connection. Without this
	// extra check, a network attacker who captured a valid
	// signed Send could replay the same envelope across a
	// different TLS session and have their own TLS leaf
	// fingerprint bound to the victim's mailbox ID — defeating
	// the post-registration fingerprint defense at the entry
	// gate (issue #448). The TLS-binding signature is a Schnorr
	// signature by the mailbox key over a BIP-340 tagged
	// digest of (senderPubKey || leafSPKI), carried in the
	// x-mailbox-tls-bind-sig envelope header.
	//
	// Missing binding signatures are rejected by default. Operators
	// with pre-#448 clients can temporarily set
	// Mailbox.RequireTLSBindingSig to false to log-and-accept during
	// an upgrade window.
	if peerInfo, ok := extractTLSPeer(ctx); ok {
		err := s.verifyTLSBindingSig(
			ctx, env, senderPubKey, peerInfo,
		)
		if err != nil {
			return err
		}

		s.bindMailboxTLS(ctx, env.Sender, peerInfo)
	}

	// Build an in-process edge client for the new client's
	// runtime. Each runtime gets its own edge instance backed
	// by the shared mailbox store.
	edgeClient, err := NewLocalMailboxClient(s.mailboxStore)
	if err != nil {
		return fmt.Errorf("build edge for %q: %w", clientID, err)
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
		return fmt.Errorf("auto-register client %q: %w", clientID, err)
	}

	s.log.InfoS(ctx, "Auto-registered external client",
		"client_id", string(clientID),
		"local_mailbox", compoundMBID,
		"remote_mailbox", env.Sender,
	)

	return nil
}

// verifyTLSBindingSig enforces that the envelope carries a Schnorr
// signature, by the same secp256k1 mailbox key whose Schnorr auth
// signature we just verified, binding that identity to the
// SubjectPublicKeyInfo of the TLS leaf we actually observed on this
// connection. Returns an error iff RequireTLSBindingSig is enabled and the
// binding signature is missing or invalid; otherwise logs the issue and returns
// nil so explicitly configured soft-rollout deployments can accept pre-#448
// clients during an upgrade window.
//
// The verification deliberately uses peerInfo.SPKI (sourced from
// the TLS PeerCertificates exposed by the gRPC transport, NOT from
// any envelope field) so a malicious client cannot supply both the
// SPKI it claims to have signed AND a matching signature; the
// server hashes whatever leaf it actually saw, full stop.
func (s *Server) verifyTLSBindingSig(ctx context.Context,
	env *mailboxpb.Envelope, senderPubKey *btcec.PublicKey,
	peerInfo tlsPeerInfo) error {

	requireBinding := s.cfg != nil && s.cfg.Mailbox != nil &&
		s.cfg.Mailbox.RequireTLSBindingSig

	bindSigHex := env.Headers[serverconn.TLSBindHeaderKey]
	if bindSigHex == "" {
		if requireBinding {
			return fmt.Errorf("missing %s header from client %q",
				serverconn.TLSBindHeaderKey, env.Sender)
		}

		s.log.WarnS(ctx,
			"TLS-binding signature missing during "+
				"first-contact registration (soft "+
				"rollout: accepted)",
			nil,
			"sender", env.Sender,
			"cn", peerInfo.SubjectCN,
		)

		return nil
	}

	if len(peerInfo.SPKI) == 0 {

		// Mailbox auth verification already happened, so we
		// know the secp256k1 key is genuine; the only way SPKI
		// can be empty here is a TLS-info extraction bug.
		// Surface it loudly rather than silently skip the
		// binding-sig check.
		return fmt.Errorf("internal: TLS peer info missing SPKI for " +
			"binding-sig verification")
	}

	if err := serverconn.VerifyMailboxTLSBind(
		senderPubKey, peerInfo.SPKI, bindSigHex,
	); err != nil {
		// Bad sig is treated as an attack signal regardless of
		// soft-rollout: a legacy client would omit the header
		// entirely, not send a malformed one.
		return fmt.Errorf("verify tls-binding sig for client %q: %w",
			env.Sender, err)
	}

	return nil
}

// bindMailboxTLS records the TLS certificate fingerprint observed on
// the connection for the bare client mailbox ID (the form Send uses in
// Envelope.Sender). The auth interceptor reduces any compound
// "operator:client" mailbox ID down to the bare client identity via
// ResolveMailboxClientID before lookup, so binding only the bare form is
// sufficient to authorize Send, Pull, and AckUpTo against the single
// Schnorr-verified registration.
//
// Bind is called only after Schnorr verification has succeeded, so an
// updated fingerprint here represents a legitimate TLS-key rotation by
// the verified identity rather than an attacker overwrite.
func (s *Server) bindMailboxTLS(ctx context.Context, senderID string,
	peerInfo tlsPeerInfo) {

	if s.mailboxTLSBindings == nil {
		return
	}

	bound := s.mailboxTLSBindings.Bind(senderID, peerInfo.Fingerprint)

	if bound {
		s.log.InfoS(ctx, "Bound TLS cert to mailbox identity",
			"sender", senderID,
			"cn", peerInfo.SubjectCN,
			"fingerprint", peerInfo.Fingerprint,
		)
	}
}

// Compile-time interface check.
var _ clientconn.UnknownClientHandler = (*Server)(nil)
