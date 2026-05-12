package darepo

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"strings"

	"github.com/btcsuite/btclog/v2"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// newMailboxAuthInterceptor returns a gRPC unary server interceptor
// that enforces per-RPC identity matching for mailbox operations. When
// the client presents a TLS client certificate, the interceptor
// extracts the Subject CommonName (which carries the client's
// secp256k1 pubkey hex) and verifies it matches the mailbox ID in
// Send, Pull, and AckUpTo requests.
//
// When requireTLS is true (production), the interceptor rejects
// mailbox RPCs that lack a TLS client certificate. When false
// (regtest/dev), requests without TLS peer info pass through, relying
// on the Schnorr auth signature in HandleUnknownClient for identity
// verification.
//
// NOTE: The TLS client certificate is self-signed with the secp256k1
// pubkey hex as the Subject CN. This CN is NOT cryptographically
// bound to the secp256k1 key — any party who knows a client's pubkey
// could forge a certificate with that CN. The Schnorr auth during
// initial registration provides the real identity proof; the mTLS
// layer is defense-in-depth for per-RPC access control after
// registration.
func newMailboxAuthInterceptor(log btclog.Logger,
	requireTLS bool) grpc.UnaryServerInterceptor {

	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {

		// Extract TLS peer identity.
		clientIdentity, ok := extractTLSIdentity(ctx)
		if !ok {
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

		// Enforce identity matching for mailbox RPCs.
		// Extract the claimed mailbox ID from the
		// request, then verify it matches the TLS
		// identity.
		var (
			rpcName   string
			claimedID string
		)

		switch typedReq := req.(type) {
		case *mailboxpb.SendRequest:
			if typedReq.Envelope == nil {
				return nil, status.Errorf(codes.InvalidArgument,
					"send request missing envelope")
			}

			rpcName = "Send"
			claimedID = typedReq.Envelope.Sender

		case *mailboxpb.PullRequest:
			rpcName = "Pull"
			claimedID = typedReq.MailboxId

		case *mailboxpb.AckUpToRequest:
			rpcName = "AckUpTo"
			claimedID = typedReq.MailboxId
		}

		if err := checkMailboxIdentity(
			ctx, log, rpcName, claimedID, clientIdentity,
		); err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

// checkMailboxIdentity verifies that the claimed mailbox ID belongs
// to the TLS-authenticated client. The claimedID may be either the
// client's bare pubkey hex (for Send) or a compound key
// "operator:client" (for Pull/AckUpTo). In both cases the client
// portion must match the TLS identity. If rpcName is empty (no
// mailbox RPC matched), it returns nil immediately.
func checkMailboxIdentity(ctx context.Context, log btclog.Logger,
	rpcName string, claimedID, clientIdentity string) error {

	if rpcName == "" {
		return nil
	}

	normalizedClaim := strings.ToLower(claimedID)

	// Direct match: the claimed ID is the client's bare
	// pubkey (used by Send where env.Sender is the client
	// identity).
	if normalizedClaim == clientIdentity {
		return nil
	}

	// Compound match: the claimed ID is "operator:client"
	// (used by Pull/AckUpTo). Verify the client portion
	// after the colon matches the TLS identity.
	if idx := strings.LastIndex(normalizedClaim, ":"); idx >= 0 {
		clientPart := normalizedClaim[idx+1:]
		if clientPart == clientIdentity {
			return nil
		}
	}

	log.WarnS(ctx, "Rejected "+rpcName+": identity mismatch",
		nil,
		slog.String("claimed", claimedID),
		slog.String("actual", clientIdentity),
	)

	return status.Errorf(codes.PermissionDenied, "mailbox identity "+
		"mismatch")
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

		// All mailbox RPCs are unary. Reject any
		// streaming RPCs on the mailbox service path
		// until stream-level identity checking is
		// implemented.
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
// client-facing gRPC server. The returned config requests (but does
// not require) client certificates, enabling the mTLS interceptor to
// enforce per-RPC identity when clients present one.
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

// extractTLSIdentity retrieves the client's identity (Subject CN)
// from the TLS peer certificate. Returns false if no TLS info or
// client certificate is available.
func extractTLSIdentity(ctx context.Context) (string, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		return "", false
	}

	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", false
	}

	if len(tlsInfo.State.PeerCertificates) == 0 {
		return "", false
	}

	cn := tlsInfo.State.PeerCertificates[0].Subject.CommonName
	if cn == "" {
		return "", false
	}

	// Normalize to lowercase for case-insensitive hex
	// comparison with mailbox IDs derived from pubkeys.
	return strings.ToLower(cn), true
}
