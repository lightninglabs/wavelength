package darepo

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/btcsuite/btclog/v2"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/serverconn"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const gatewayAuthMetadataKey = "x-darepo-internal-gateway-auth"

// mailboxTLSBindings tracks the binding between a Schnorr-authenticated
// secp256k1 mailbox identity and the TLS leaf certificate fingerprint
// that the client presented during its first authenticated contact.
//
// The TLS interceptor uses this map (rather than the certificate
// Subject CommonName) to authorize per-RPC mailbox access. CN-based
// matching is fundamentally forgeable because:
//
//  1. The server uses tls.RequestClientCert (no CA chain validation),
//     so any self-signed cert is accepted at the TLS layer.
//  2. The CN is free text the client picks at certificate generation
//     time. Nothing in the TLS handshake binds the CN to the
//     secp256k1 identity it claims.
//
// A fingerprint binding closes the gap: the TLS handshake proves the
// client holds the private key for the presented certificate (via the
// CertificateVerify signature), and the Schnorr auth verified during
// HandleUnknownClient proves the client holds the secp256k1 private
// key for the claimed mailbox ID. Storing the fingerprint observed on
// the Schnorr-verified connection ties future TLS sessions to the
// original verified secp256k1 identity. A subsequent attacker who
// merely knows the victim's mailbox ID cannot satisfy this binding
// because they do not hold the TLS private key for the registered
// certificate.
type mailboxTLSBindings struct {
	mu sync.RWMutex

	// fingerprints maps a normalized mailbox ID to the SHA-256
	// fingerprint of the leaf TLS certificate observed when the
	// identity was first Schnorr-verified.
	fingerprints map[string]string
}

// newMailboxTLSBindings returns an initialized binding registry.
func newMailboxTLSBindings() *mailboxTLSBindings {
	return &mailboxTLSBindings{
		fingerprints: make(map[string]string),
	}
}

// Bind records the TLS certificate fingerprint for a mailbox ID. If a
// binding already exists for the ID, it is updated if the fingerprint
// differs. This is safe because Bind is only called after successful
// Schnorr verification: the caller has cryptographically proven
// possession of the identity's private key, so a fresh registration
// with a rotated TLS cert is a legitimate update rather than an
// attacker overwrite.
//
// Returns true if the binding was created or updated, false if the
// existing binding already matched the supplied fingerprint.
func (b *mailboxTLSBindings) Bind(mailboxID, fingerprint string) bool {
	if mailboxID == "" || fingerprint == "" {
		return false
	}

	key := strings.ToLower(mailboxID)

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.fingerprints[key] == fingerprint {
		return false
	}

	b.fingerprints[key] = fingerprint

	return true
}

// Lookup returns the TLS certificate fingerprint bound to the given
// mailbox ID, or ("", false) if no binding exists.
func (b *mailboxTLSBindings) Lookup(mailboxID string) (string, bool) {
	if mailboxID == "" {
		return "", false
	}

	key := strings.ToLower(mailboxID)

	b.mu.RLock()
	defer b.mu.RUnlock()

	fp, ok := b.fingerprints[key]

	return fp, ok
}

// certFingerprint returns the lowercase hex SHA-256 of the leaf TLS
// certificate's DER bytes. SHA-256 of the raw DER is a stable
// connection-independent identifier for the certificate that an
// attacker cannot replicate without holding the certificate's private
// key (since the TLS handshake's CertificateVerify message proves
// possession).
func certFingerprint(cert *x509.Certificate) string {
	if cert == nil || len(cert.Raw) == 0 {
		return ""
	}

	sum := sha256.Sum256(cert.Raw)

	return hex.EncodeToString(sum[:])
}

// tlsPeerInfo carries the verified TLS context extracted from an
// incoming RPC: the leaf certificate fingerprint (proved-possessed via
// the TLS handshake) and the Subject CommonName (informational only,
// used for diagnostics — never for authorization).
type tlsPeerInfo struct {
	// Fingerprint is the SHA-256 hex of the leaf certificate DER.
	// This is the authorization key for per-RPC identity checks.
	Fingerprint string

	// SubjectCN is the leaf certificate's Subject CommonName. It
	// is recorded for log diagnostics only; the CN MUST NOT be
	// used as an identity claim because it is unbound to any
	// verified secret.
	SubjectCN string

	// SPKI is the DER-encoded SubjectPublicKeyInfo of the leaf
	// certificate. It is the message a first-contact client
	// must sign with their secp256k1 mailbox key to bind their
	// identity to this TLS leaf (issue #448). SPKI is preferred
	// over the raw key bytes because it commits to the curve
	// and algorithm identifier in addition to the key material.
	SPKI []byte
}

// extractTLSPeer returns the leaf TLS certificate fingerprint and CN
// from the RPC context, or (zero, false) if the request did not arrive
// over a TLS connection that presented a client certificate.
func extractTLSPeer(ctx context.Context) (tlsPeerInfo, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		return tlsPeerInfo{}, false
	}

	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return tlsPeerInfo{}, false
	}

	if len(tlsInfo.State.PeerCertificates) == 0 {
		return tlsPeerInfo{}, false
	}

	leaf := tlsInfo.State.PeerCertificates[0]
	fp := certFingerprint(leaf)
	if fp == "" {
		return tlsPeerInfo{}, false
	}

	return tlsPeerInfo{
		Fingerprint: fp,
		SubjectCN:   leaf.Subject.CommonName,
		SPKI:        leaf.RawSubjectPublicKeyInfo,
	}, true
}

// newMailboxAuthInterceptor returns a gRPC unary server interceptor
// that enforces per-RPC identity matching for mailbox operations.
//
// The interceptor's security model:
//
//  1. Send is permitted to flow through to the autoRegisteringMailbox
//     wrapper, which calls HandleInbound. HandleInbound verifies the
//     envelope's Schnorr auth signature and, on success, binds the
//     sender's secp256k1 mailbox ID to the TLS leaf certificate
//     fingerprint observed on the connection. The mTLS layer enforces
//     that Send envelopes carry a Sender matching the TLS fingerprint
//     when a binding already exists, preventing a forged cert from
//     impersonating a previously-registered client.
//
//  2. Pull and AckUpTo require an existing binding: the presented TLS
//     leaf cert fingerprint must match the fingerprint recorded for
//     the claimed mailbox ID at Schnorr-verified registration time. A
//     forged cert with the victim's CN will not match the registered
//     fingerprint and is rejected.
//
// When requireTLS is true (production), the interceptor rejects
// mailbox RPCs that lack a TLS client certificate. When false
// (regtest/dev), requests without TLS peer info pass through, relying
// on the Schnorr auth signature in HandleInbound for identity
// verification.
//
// The Subject CommonName is intentionally NOT consulted for
// authorization. The CN is unverified free text and was previously the
// source of the forged-identity vulnerability tracked in
// darepo issue #362.
func newMailboxAuthInterceptor(log btclog.Logger, bindings *mailboxTLSBindings,
	requireTLS bool, gatewayAuthToken string) grpc.UnaryServerInterceptor {

	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {

		// Extract TLS peer info (fingerprint + CN for logs).
		peerInfo, hasTLS := extractTLSPeer(ctx)
		if !hasTLS {
			if hasGatewayAuth(ctx, gatewayAuthToken) {
				authed, err := verifyMailboxMetadataAuth(
					ctx, req,
				)
				if err != nil {
					return nil, err
				}
				if authed {
					return handler(ctx, req)
				}
			}

			// When TLS is required, reject mailbox RPCs
			// without a client certificate.
			if requireTLS && isMailboxRPC(req) {
				log.WarnS(ctx,
					"Rejected mailbox RPC: no "+
						"TLS client certificate",
					nil,
				)

				return nil, status.Errorf(codes.Unauthenticated,
					"TLS client certificate required for "+
						"mailbox operations")
			}

			// WARNING: non-TLS mode has no per-RPC
			// enforcement. The Schnorr auth signature
			// provides identity verification only during
			// initial registration.
			return handler(ctx, req)
		}

		// Resolve the claimed mailbox ID for the request. For
		// non-mailbox RPCs, fall through without identity
		// checks.
		rpcName, claimedID, err := classifyMailboxRequest(req)
		if err != nil {
			return nil, err
		}

		if rpcName == "" {
			return handler(ctx, req)
		}

		if err := checkMailboxIdentity(
			ctx, log, rpcName, claimedID, peerInfo, bindings,
		); err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

// newGatewayAuthToken returns an unguessable per-process token used to mark
// requests that arrived through the local HTTP gateway.
func newGatewayAuthToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate gateway auth token: %w", err)
	}

	return hex.EncodeToString(token[:]), nil
}

// hasGatewayAuth reports whether this inbound gRPC request was emitted by the
// in-process grpc-gateway dialer.
func hasGatewayAuth(ctx context.Context, token string) bool {
	if token == "" {
		return false
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}

	values := md.Get(gatewayAuthMetadataKey)

	return len(values) == 1 && values[0] == token
}

// verifyMailboxMetadataAuth checks browser-compatible mailbox auth metadata.
func verifyMailboxMetadataAuth(ctx context.Context, req any) (bool, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false, nil
	}

	values := md.Get(serverconn.AuthHeaderKey)
	if len(values) == 0 || values[0] == "" {
		return false, nil
	}
	authSig := values[0]

	var (
		senderID    string
		recipientID string
	)

	switch typedReq := req.(type) {
	case *mailboxpb.SendRequest:
		if typedReq.Envelope == nil {
			return false, status.Errorf(codes.InvalidArgument,
				"send request missing envelope")
		}

		senderID = typedReq.Envelope.Sender
		recipientID = typedReq.Envelope.Recipient

	case *mailboxpb.PullRequest:
		recipientID = typedReq.MailboxId
		senderID = claimedClientID(recipientID)

	case *mailboxpb.AckUpToRequest:
		recipientID = typedReq.MailboxId
		senderID = claimedClientID(recipientID)

	default:
		return false, nil
	}

	senderPubKey, err := serverconn.ParseMailboxPubKey(senderID)
	if err != nil {
		return false, status.Errorf(codes.Unauthenticated, "invalid "+
			"mailbox auth identity: %v", err)
	}

	if err := serverconn.VerifyMailboxAuth(
		senderPubKey, recipientID, authSig,
	); err != nil {
		return false, status.Errorf(codes.Unauthenticated, "invalid "+
			"mailbox auth signature: %v", err)
	}

	return true, nil
}

// classifyMailboxRequest returns the RPC name and the mailbox ID that
// the request is claiming to operate on. Returns an empty rpcName for
// non-mailbox requests so the caller can short-circuit. Returns an
// InvalidArgument error for structurally invalid mailbox requests
// (e.g. a SendRequest with a nil envelope) to keep the validation in
// one place.
func classifyMailboxRequest(req any) (string, string, error) {
	switch typedReq := req.(type) {
	case *mailboxpb.SendRequest:
		if typedReq.Envelope == nil {
			return "", "", status.Errorf(codes.InvalidArgument,
				"send request missing envelope")
		}

		return "Send", typedReq.Envelope.Sender, nil

	case *mailboxpb.PullRequest:
		return "Pull", typedReq.MailboxId, nil

	case *mailboxpb.AckUpToRequest:
		return "AckUpTo", typedReq.MailboxId, nil

	default:
		return "", "", nil
	}
}

// claimedClientID returns the client-identity portion of a claimed
// mailbox ID. The wire format is either:
//
//   - a bare hex-encoded compressed pubkey (used by Send.Envelope.Sender);
//   - a compound "operator:client" mailbox ID (used by Pull/AckUpTo).
//
// In the compound case the operator part is the server's mailbox ID
// and is not the entity we authorize against; only the trailing
// client part identifies the caller.
func claimedClientID(claimedID string) string {
	if idx := strings.LastIndex(claimedID, ":"); idx >= 0 {
		return claimedID[idx+1:]
	}

	return claimedID
}

// checkMailboxIdentity verifies that the TLS certificate presented on
// the connection is the one bound to the claimed mailbox ID at
// Schnorr-verified registration time.
//
// The function distinguishes three cases:
//
//  1. Send with no existing binding: allowed. The Send envelope flows
//     into HandleInbound, which performs the Schnorr verification and
//     creates the binding. Refusing here would prevent any client
//     from ever registering.
//
//  2. Send with an existing binding: the presenting cert MUST match
//     the bound fingerprint. This prevents an attacker who learns a
//     registered mailbox ID from injecting Send envelopes that
//     impersonate the registered client.
//
//  3. Pull / AckUpTo: a binding MUST already exist and MUST match the
//     presenting fingerprint. Pull and AckUpTo expose the mailbox
//     contents and the read cursor, so they have no pre-registration
//     entry point; they only make sense for a previously
//     Schnorr-authenticated identity.
func checkMailboxIdentity(ctx context.Context, log btclog.Logger, rpcName,
	claimedID string, peerInfo tlsPeerInfo,
	bindings *mailboxTLSBindings) error {

	if rpcName == "" {
		return nil
	}

	clientID := claimedClientID(claimedID)
	if clientID == "" {
		log.WarnS(ctx, "Rejected "+rpcName+": empty mailbox ID",
			nil,
			slog.String("claimed", claimedID),
		)

		return status.Errorf(codes.InvalidArgument, "mailbox ID "+
			"required")
	}

	bound, hasBinding := bindings.Lookup(clientID)

	switch rpcName {
	case "Send":
		// First contact: allow through so HandleInbound can
		// verify the Schnorr auth signature and create the
		// binding.
		if !hasBinding {
			return nil
		}

	case "Pull", "AckUpTo":
		// Read paths must never operate on an unverified
		// identity. Without a binding there is no proof that
		// the caller's TLS key is tied to the secp256k1
		// identity they are reading on behalf of.
		if !hasBinding {
			log.WarnS(ctx,
				"Rejected "+rpcName+": no Schnorr-verified "+
					"binding for claimed mailbox ID",
				nil,
				slog.String("claimed", claimedID),
				slog.String("cn", peerInfo.SubjectCN),
			)

			return status.Errorf(codes.PermissionDenied,
				"mailbox identity not registered")
		}
	}

	if bound != peerInfo.Fingerprint {
		log.WarnS(ctx,
			"Rejected "+rpcName+": TLS cert fingerprint does not "+
				"match registered binding", nil,
			slog.String("claimed", claimedID),
			slog.String("cn", peerInfo.SubjectCN),
		)

		return status.Errorf(codes.PermissionDenied, "mailbox "+
			"identity mismatch")
	}

	return nil
}

// isMailboxRPC returns true if the request is a mailbox operation
// (Send, Pull, or AckUpTo).
func isMailboxRPC(req any) bool {
	switch req.(type) {
	case *mailboxpb.SendRequest, *mailboxpb.PullRequest,
		*mailboxpb.AckUpToRequest:
		return true

	default:
		return false
	}
}

// newMailboxStreamInterceptor returns a gRPC stream interceptor that
// rejects streaming RPCs on the mailbox service. All mailbox RPCs
// are currently unary; this interceptor is fail-closed to prevent
// future streaming RPCs from silently bypassing identity checks.
func newMailboxStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo,
		handler grpc.StreamHandler) error {

		// All mailbox RPCs are unary. Reject any streaming RPCs
		// on the mailbox service path until stream-level
		// identity checking is implemented.
		if strings.Contains(
			info.FullMethod, "MailboxService",
		) {
			return status.Errorf(codes.Unimplemented, "streaming "+
				"not supported for mailbox RPCs")
		}

		return handler(srv, ss)
	}
}

// loadServerTLSConfig loads the TLS certificate and key from the
// configured paths and returns a tls.Config suitable for the
// client-facing gRPC server.
//
// The server requests (but does not require) client certificates so
// that the mTLS interceptor can enforce per-RPC identity for clients
// that present one. Because clients ship self-signed certs and the
// server does not pin a CA, the TLS layer itself cannot validate the
// claimed identity — that binding is supplied by the
// mailboxTLSBindings registry which is populated only after Schnorr
// authentication in HandleUnknownClient.
func loadServerTLSConfig(cfg *TLSConfig) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("load TLS keypair: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{
			cert,
		},
		ClientAuth: tls.RequestClientCert,
		MinVersion: tls.VersionTLS12,
	}, nil
}
