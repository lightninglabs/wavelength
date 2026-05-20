package darepo

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// makeTestCert generates a fresh self-signed P-256 TLS certificate
// with the given Subject CommonName. The returned certificate is what
// a real client would present on the wire; tests use this to exercise
// the interceptor's fingerprint-binding logic without trusting the CN.
func makeTestCert(t *testing.T, cn string) *x509.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(
		rand.Reader,
		new(
			big.Int).Lsh(big.NewInt(1), 128),
	)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: cn,
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
		},
	}

	der, err := x509.CreateCertificate(
		rand.Reader, tmpl, tmpl, &key.PublicKey, key,
	)
	require.NoError(t, err)

	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return leaf
}

// tlsPeerCtx returns a context with TLS peer info carrying the given
// already-generated client certificate.
func tlsPeerCtx(t *testing.T, cert *x509.Certificate) context.Context {
	t.Helper()

	p := &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{
					cert,
				},
			},
		},
	}

	return peer.NewContext(t.Context(), p)
}

// passHandler is a grpc.UnaryHandler that always succeeds.
func passHandler(_ context.Context, _ any) (any, error) {
	return "ok", nil
}

// TestMailboxAuthInterceptor exercises the interceptor's authorization
// matrix: TLS present/absent, require/allow, Send vs. Pull/AckUpTo,
// and the critical forged-CN rejection that closes issue #362.
func TestMailboxAuthInterceptor(t *testing.T) {
	t.Parallel()

	const (
		alicePK      = "02abc123"
		bobPK        = "03def456"
		gatewayToken = "test-gateway-token"
	)

	// aliceCert is the cert Alice was holding when her Schnorr
	// auth was verified; its fingerprint is the value bound in the
	// registry. forgedAlice carries Alice's mailbox ID in the CN
	// but is a freshly-generated cert with a different key (and
	// therefore a different fingerprint), modelling the attacker.
	aliceCert := makeTestCert(t, alicePK)
	forgedAlice := makeTestCert(t, alicePK)
	bobCert := makeTestCert(t, bobPK)

	aliceFP := certFingerprint(aliceCert)

	// Helper to build a registry pre-bound to Alice.
	withAliceBound := func() *mailboxTLSBindings {
		b := newMailboxTLSBindings()
		b.Bind(alicePK, aliceFP)

		return b
	}

	tests := []struct {
		name       string
		bindings   func() *mailboxTLSBindings
		requireTLS bool
		makeCtx    func(t *testing.T) context.Context
		req        any
		wantCode   codes.Code
		wantPass   bool
	}{
		{
			name:       "no TLS, no require — pass",
			bindings:   newMailboxTLSBindings,
			requireTLS: false,
			makeCtx: func(t *testing.T) context.Context {
				return t.Context()
			},
			req: &mailboxpb.PullRequest{
				MailboxId: alicePK,
			},
			wantPass: true,
		},
		{
			name:       "no TLS, require — reject",
			bindings:   newMailboxTLSBindings,
			requireTLS: true,
			makeCtx: func(t *testing.T) context.Context {
				return t.Context()
			},
			req: &mailboxpb.PullRequest{
				MailboxId: alicePK,
			},
			wantCode: codes.Unauthenticated,
		},
		{
			name:       "no TLS, require — non-mailbox",
			bindings:   newMailboxTLSBindings,
			requireTLS: true,
			makeCtx: func(t *testing.T) context.Context {
				return t.Context()
			},
			req:      "some-other-request",
			wantPass: true,
		},
		{
			name:     "Send first contact — pass and binding-free",
			bindings: newMailboxTLSBindings,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, aliceCert)
			},
			req: &mailboxpb.SendRequest{
				Envelope: &mailboxpb.Envelope{
					Sender: alicePK,
				},
			},
			wantPass: true,
		},
		{
			name:     "Pull without binding — reject",
			bindings: newMailboxTLSBindings,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, aliceCert)
			},
			req: &mailboxpb.PullRequest{
				MailboxId: alicePK,
			},
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "AckUpTo without binding — reject",
			bindings: newMailboxTLSBindings,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, aliceCert)
			},
			req: &mailboxpb.AckUpToRequest{
				MailboxId: alicePK,
			},
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "Pull bound match — pass",
			bindings: withAliceBound,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, aliceCert)
			},
			req: &mailboxpb.PullRequest{
				MailboxId: alicePK,
			},
			wantPass: true,
		},
		{
			name:     "Pull forged cert with matching CN — reject",
			bindings: withAliceBound,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, forgedAlice)
			},
			req: &mailboxpb.PullRequest{
				MailboxId: alicePK,
			},
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "AckUpTo bound match — pass",
			bindings: withAliceBound,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, aliceCert)
			},
			req: &mailboxpb.AckUpToRequest{
				MailboxId: alicePK,
			},
			wantPass: true,
		},
		{
			name:     "AckUpTo forged cert — reject",
			bindings: withAliceBound,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, forgedAlice)
			},
			req: &mailboxpb.AckUpToRequest{
				MailboxId: alicePK,
			},
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "Pull cross-tenant — reject",
			bindings: withAliceBound,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, bobCert)
			},
			req: &mailboxpb.PullRequest{
				MailboxId: alicePK,
			},
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "Send fingerprint mismatch — reject",
			bindings: withAliceBound,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, forgedAlice)
			},
			req: &mailboxpb.SendRequest{
				Envelope: &mailboxpb.Envelope{
					Sender: alicePK,
				},
			},
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "Pull compound match",
			bindings: withAliceBound,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, aliceCert)
			},
			req: &mailboxpb.PullRequest{
				MailboxId: bobPK + ":" + alicePK,
			},
			wantPass: true,
		},
		{
			name:     "Pull compound forged",
			bindings: withAliceBound,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, forgedAlice)
			},
			req: &mailboxpb.PullRequest{
				MailboxId: bobPK + ":" + alicePK,
			},
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "nil envelope — InvalidArgument",
			bindings: newMailboxTLSBindings,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, aliceCert)
			},
			req: &mailboxpb.SendRequest{
				Envelope: nil,
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name:       "non-mailbox with TLS — pass",
			bindings:   newMailboxTLSBindings,
			requireTLS: true,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, aliceCert)
			},
			req:      "some-other-request",
			wantPass: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			interceptor := newMailboxAuthInterceptor(
				btclog.Disabled, tc.bindings(), tc.requireTLS,
				gatewayToken,
			)

			resp, err := interceptor(
				tc.makeCtx(t), tc.req, &grpc.UnaryServerInfo{},
				passHandler,
			)

			if tc.wantPass {
				require.NoError(t, err)
				require.Equal(t, "ok", resp)
			} else {
				require.Error(t, err)

				st, ok := status.FromError(err)
				require.True(t, ok,
					"expected gRPC status",
				)
				require.Equal(t,
					tc.wantCode, st.Code(),
				)
			}
		})
	}
}

// TestMailboxAuthInterceptorAcceptsMetadata verifies browser clients can
// authenticate mailbox RPCs through forwarded HTTP metadata.
func TestMailboxAuthInterceptorAcceptsMetadata(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKey := privKey.PubKey()
	clientID := serverconn.PubKeyMailboxID(pubKey)
	recipientID := "operator:" + clientID

	authSig, err := serverconn.SignMailboxAuth(privKey, recipientID)
	require.NoError(t, err)

	ctx := metadata.NewIncomingContext(
		t.Context(),
		metadata.Pairs(
			gatewayAuthMetadataKey, "test-gateway-token",
			serverconn.AuthHeaderKey,
			hex.EncodeToString(
				authSig.Serialize(),
			),
		),
	)

	interceptor := newMailboxAuthInterceptor(
		btclog.Disabled, newMailboxTLSBindings(), true,
		"test-gateway-token",
	)
	resp, err := interceptor(
		ctx, &mailboxpb.PullRequest{
			MailboxId: recipientID,
		}, &grpc.UnaryServerInfo{}, passHandler,
	)
	require.NoError(t, err)
	require.Equal(t, "ok", resp)
}

// TestMailboxAuthInterceptorRejectsBadMetadata verifies bad gateway metadata
// cannot bypass mailbox identity checks.
func TestMailboxAuthInterceptorRejectsBadMetadata(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	otherKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientID := serverconn.PubKeyMailboxID(privKey.PubKey())
	recipientID := "operator:" + clientID

	authSig, err := serverconn.SignMailboxAuth(otherKey, recipientID)
	require.NoError(t, err)

	ctx := metadata.NewIncomingContext(
		t.Context(),
		metadata.Pairs(
			gatewayAuthMetadataKey, "test-gateway-token",
			serverconn.AuthHeaderKey,
			hex.EncodeToString(
				authSig.Serialize(),
			),
		),
	)

	interceptor := newMailboxAuthInterceptor(
		btclog.Disabled, newMailboxTLSBindings(), true,
		"test-gateway-token",
	)
	_, err = interceptor(
		ctx, &mailboxpb.PullRequest{
			MailboxId: recipientID,
		}, &grpc.UnaryServerInfo{}, passHandler,
	)
	require.Error(t, err)

	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Unauthenticated, st.Code())
}

// TestMailboxAuthInterceptorRejectsMetadataWithoutGatewayAuth verifies direct
// gRPC callers cannot use browser mailbox metadata to bypass mTLS.
func TestMailboxAuthInterceptorRejectsMetadataWithoutGatewayAuth(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientID := serverconn.PubKeyMailboxID(privKey.PubKey())
	recipientID := "operator:" + clientID

	authSig, err := serverconn.SignMailboxAuth(privKey, recipientID)
	require.NoError(t, err)

	ctx := metadata.NewIncomingContext(
		t.Context(),
		metadata.Pairs(
			serverconn.AuthHeaderKey,
			hex.EncodeToString(
				authSig.Serialize(),
			),
		),
	)

	interceptor := newMailboxAuthInterceptor(
		btclog.Disabled, newMailboxTLSBindings(), true,
		"test-gateway-token",
	)
	_, err = interceptor(
		ctx, &mailboxpb.PullRequest{
			MailboxId: recipientID,
		}, &grpc.UnaryServerInfo{}, passHandler,
	)
	require.Error(t, err)

	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Unauthenticated, st.Code())
}

// TestMailboxTLSBindingsRotationAfterVerification verifies that once a
// binding is recorded for a mailbox ID, a later Bind with a different
// fingerprint updates the binding. This is safe because Bind is only
// called from the auto-register path after Schnorr verification has
// proven possession of the identity's private key, so a fresh cert
// represents a legitimate TLS-key rotation. Re-binding with the same
// fingerprint is a no-op (returns false).
func TestMailboxTLSBindingsRotationAfterVerification(t *testing.T) {
	t.Parallel()

	const id = "02abc"

	b := newMailboxTLSBindings()

	// Initial registration creates the binding.
	require.True(t, b.Bind(id, "fp-legit"))

	// Re-binding with the same fingerprint is a no-op.
	require.False(t, b.Bind(id, "fp-legit"))

	// A verified re-registration with a rotated cert updates the
	// binding to the new fingerprint.
	require.True(t, b.Bind(id, "fp-rotated"))

	got, ok := b.Lookup(id)
	require.True(t, ok)
	require.Equal(t, "fp-rotated", got)
}

// TestMailboxTLSBindingsCaseInsensitive verifies that mailbox ID
// lookup is case-insensitive, matching the normalization used by the
// interceptor for hex-encoded pubkey comparison.
func TestMailboxTLSBindingsCaseInsensitive(t *testing.T) {
	t.Parallel()

	b := newMailboxTLSBindings()
	require.True(t, b.Bind("02ABCdef", "fp"))

	got, ok := b.Lookup("02abcdef")
	require.True(t, ok)
	require.Equal(t, "fp", got)
}

// TestCertFingerprintStable verifies that two parses of the same DER
// produce the same fingerprint, and that two independently generated
// certs with the same CN produce different fingerprints. The latter
// is what makes a CN-based forgery detectable.
func TestCertFingerprintStable(t *testing.T) {
	t.Parallel()

	cert1 := makeTestCert(t, "02abc")
	cert2 := makeTestCert(t, "02abc")

	require.NotEmpty(t, certFingerprint(cert1))
	require.Equal(t, certFingerprint(cert1), certFingerprint(cert1))
	require.NotEqual(t, certFingerprint(cert1), certFingerprint(cert2))
}

// TestExtractTLSPeer verifies that extractTLSPeer returns the leaf
// certificate fingerprint and CN, and handles all the edge cases
// (no peer, nil auth, no certs).
func TestExtractTLSPeer(t *testing.T) {
	t.Parallel()

	t.Run("no peer info", func(t *testing.T) {
		t.Parallel()

		_, ok := extractTLSPeer(t.Context())
		require.False(t, ok)
	})

	t.Run("peer with nil AuthInfo", func(t *testing.T) {
		t.Parallel()

		ctx := peer.NewContext(t.Context(), &peer.Peer{
			AuthInfo: nil,
		})
		_, ok := extractTLSPeer(ctx)
		require.False(t, ok)
	})

	t.Run("TLS with no certs", func(t *testing.T) {
		t.Parallel()

		ctx := peer.NewContext(t.Context(), &peer.Peer{
			AuthInfo: credentials.TLSInfo{
				State: tls.ConnectionState{},
			},
		})
		_, ok := extractTLSPeer(ctx)
		require.False(t, ok)
	})

	t.Run("TLS with valid cert", func(t *testing.T) {
		t.Parallel()

		cert := makeTestCert(t, "02abc")
		info, ok := extractTLSPeer(tlsPeerCtx(t, cert))
		require.True(t, ok)
		require.Equal(t, certFingerprint(cert), info.Fingerprint)
		require.Equal(t, "02abc", info.SubjectCN)
		require.NotEmpty(t, info.SPKI)
		require.Equal(t, cert.RawSubjectPublicKeyInfo, info.SPKI)
	})
}

// TestIsMailboxRPC verifies classification of requests as mailbox
// RPCs (Send, Pull, AckUpTo) versus non-mailbox requests.
func TestIsMailboxRPC(t *testing.T) {
	t.Parallel()

	require.True(t, isMailboxRPC(
		&mailboxpb.SendRequest{},
	))
	require.True(t, isMailboxRPC(
		&mailboxpb.PullRequest{},
	))
	require.True(t, isMailboxRPC(
		&mailboxpb.AckUpToRequest{},
	))
	require.False(t, isMailboxRPC("not-mailbox"))
	require.False(t, isMailboxRPC(nil))
}
