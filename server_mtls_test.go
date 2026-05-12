package darepo

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"testing"

	"github.com/btcsuite/btclog/v2"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// tlsPeerCtx returns a context with TLS peer info containing a
// client certificate with the given CN.
func tlsPeerCtx(t *testing.T, cn string) context.Context {
	t.Helper()

	p := &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{
					{
						Subject: pkix.Name{
							CommonName: cn,
						},
					},
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

// TestMailboxAuthInterceptor exercises all code paths of the mailbox
// auth interceptor: TLS present/absent, require/allow, and identity
// match/mismatch for each RPC type.
func TestMailboxAuthInterceptor(t *testing.T) {
	t.Parallel()

	const (
		alicePK = "02abc123"
		bobPK   = "03def456"
	)

	tests := []struct {
		name       string
		requireTLS bool
		makeCtx    func(t *testing.T) context.Context
		req        any
		wantCode   codes.Code
		wantPass   bool
	}{
		{
			name:       "no TLS, no require — pass",
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
			requireTLS: true,
			makeCtx: func(t *testing.T) context.Context {
				return t.Context()
			},
			req:      "some-other-request",
			wantPass: true,
		},
		{
			name:       "TLS match Pull",
			requireTLS: false,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, alicePK)
			},
			req: &mailboxpb.PullRequest{
				MailboxId: alicePK,
			},
			wantPass: true,
		},
		{
			name:       "TLS mismatch Pull",
			requireTLS: false,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, alicePK)
			},
			req: &mailboxpb.PullRequest{
				MailboxId: bobPK,
			},
			wantCode: codes.PermissionDenied,
		},
		{
			name:       "TLS match AckUpTo",
			requireTLS: false,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, alicePK)
			},
			req: &mailboxpb.AckUpToRequest{
				MailboxId: alicePK,
			},
			wantPass: true,
		},
		{
			name:       "TLS mismatch AckUpTo",
			requireTLS: false,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, alicePK)
			},
			req: &mailboxpb.AckUpToRequest{
				MailboxId: bobPK,
			},
			wantCode: codes.PermissionDenied,
		},
		{
			name:       "TLS match Send",
			requireTLS: false,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, alicePK)
			},
			req: &mailboxpb.SendRequest{
				Envelope: &mailboxpb.Envelope{
					Sender: alicePK,
				},
			},
			wantPass: true,
		},
		{
			name:       "TLS mismatch Send",
			requireTLS: false,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, alicePK)
			},
			req: &mailboxpb.SendRequest{
				Envelope: &mailboxpb.Envelope{
					Sender: bobPK,
				},
			},
			wantCode: codes.PermissionDenied,
		},
		{
			name:       "nil envelope — InvalidArgument",
			requireTLS: false,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, alicePK)
			},
			req: &mailboxpb.SendRequest{
				Envelope: nil,
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name:       "TLS compound Pull match",
			requireTLS: false,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, alicePK)
			},
			req: &mailboxpb.PullRequest{
				MailboxId: bobPK + ":" + alicePK,
			},
			wantPass: true,
		},
		{
			name:       "TLS compound Pull mismatch",
			requireTLS: false,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, alicePK)
			},
			req: &mailboxpb.PullRequest{
				MailboxId: bobPK + ":" + bobPK,
			},
			wantCode: codes.PermissionDenied,
		},
		{
			name:       "non-mailbox with TLS — pass",
			requireTLS: true,
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, alicePK)
			},
			req:      "some-other-request",
			wantPass: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			interceptor := newMailboxAuthInterceptor(
				btclog.Disabled, tc.requireTLS,
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

// TestExtractTLSIdentity verifies that extractTLSIdentity correctly
// extracts the Subject CN from TLS peer certificates and handles
// all edge cases (no peer, nil auth, no certs, empty CN).
func TestExtractTLSIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		makeCtx func(t *testing.T) context.Context
		wantID  string
		wantOK  bool
	}{
		{
			name: "no peer info",
			makeCtx: func(t *testing.T) context.Context {
				return t.Context()
			},
			wantOK: false,
		},
		{
			name: "peer with nil AuthInfo",
			makeCtx: func(t *testing.T) context.Context {
				return peer.NewContext(
					t.Context(), &peer.Peer{
						AuthInfo: nil,
					},
				)
			},
			wantOK: false,
		},
		{
			name: "TLS with no certs",
			makeCtx: func(
				t *testing.T,
			) context.Context {

				info := credentials.TLSInfo{
					State: tls.ConnectionState{},
				}

				return peer.NewContext(
					t.Context(), &peer.Peer{
						AuthInfo: info,
					},
				)
			},
			wantOK: false,
		},
		{
			name: "TLS with empty CN",
			makeCtx: func(
				t *testing.T,
			) context.Context {

				cert := &x509.Certificate{
					Subject: pkix.Name{},
				}
				certs := []*x509.Certificate{
					cert,
				}
				info := credentials.TLSInfo{
					State: tls.ConnectionState{
						PeerCertificates: certs,
					},
				}

				return peer.NewContext(
					t.Context(), &peer.Peer{
						AuthInfo: info,
					},
				)
			},
			wantOK: false,
		},
		{
			name: "TLS with valid CN",
			makeCtx: func(t *testing.T) context.Context {
				return tlsPeerCtx(t, "02abc123")
			},
			wantID: "02abc123",
			wantOK: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			id, ok := extractTLSIdentity(
				tc.makeCtx(t),
			)
			require.Equal(t, tc.wantOK, ok)

			if tc.wantOK {
				require.Equal(t, tc.wantID, id)
			}
		})
	}
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
