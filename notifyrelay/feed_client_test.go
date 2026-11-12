package notifyrelay

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/rpcauth"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	macaroon "gopkg.in/macaroon.v2"
)

// feedRPCFixture runs a real TLS producer edge with ordinary RPC admission and
// independently verified R proof. It contains no wallet runtime registration.
type feedRPCFixture struct {
	pull func(context.Context, *pb.PullRequest) (*pb.PullResponse, error)
	ack  func(context.Context, *pb.AckUpToRequest) (
		*pb.AckUpToResponse, error)
	pb.UnimplementedMailboxServiceServer
	pb.UnimplementedWakeFeedServiceServer
	source           *feedSource
	event            *pb.Envelope
	expectedMacaroon string
	reads            atomic.Int32
	wrongPin         atomic.Bool
	emptyStarted     chan struct{}
	emptyOnce        sync.Once
}

// GetFeedChallenge checks ordinary RPC credentials without requiring R proof.
func (p *feedRPCFixture) GetFeedChallenge(ctx context.Context,
	r *pb.NotificationChallengeRequest) (*pb.NotificationChallenge, error) {

	md, _ := metadata.FromIncomingContext(ctx)
	if len(md.Get(serverconn.AuthHeaderKey)) != 0 {
		return nil, status.Error(
			codes.InvalidArgument, "probe must precede proof",
		)
	}
	if len(md.Get(rpcauth.MacaroonMetadataKey)) != 1 ||
		md.Get(rpcauth.MacaroonMetadataKey)[0] != p.expectedMacaroon {
		return nil, status.Error(
			codes.Unauthenticated, "RPC credential required",
		)
	}
	source := p.source.public.SerializeCompressed()
	if p.wrongPin.Load() {
		source = nil
	}

	return &pb.NotificationChallenge{
		OwnerPubkey:    r.OwnerPubkey,
		OperatorPubkey: source,
	}, nil
}

// proof authenticates the exact configured subscriber mailbox and credentials.
func (p *feedRPCFixture) proof(ctx context.Context, mailbox string) error {
	p.reads.Add(1)
	md, _ := metadata.FromIncomingContext(ctx)
	signatures := md.Get(serverconn.AuthHeaderKey)
	if mailbox != p.source.mailboxID || len(signatures) != 1 {
		return status.Error(
			codes.Unauthenticated, "subscriber proof required",
		)
	}
	if err := serverconn.VerifyMailboxAuth(
		p.source.subscriber.PubKey(), mailbox, signatures[0],
	); err != nil {
		return status.Error(
			codes.Unauthenticated, "subscriber proof rejected",
		)
	}
	if len(md.Get(rpcauth.MacaroonMetadataKey)) != 1 ||
		md.Get(rpcauth.MacaroonMetadataKey)[0] != p.expectedMacaroon {
		return status.Error(
			codes.Unauthenticated, "RPC credential required",
		)
	}

	return nil
}

// Pull returns one fixed event through the real protobuf and TLS transport.
func (p *feedRPCFixture) Pull(ctx context.Context, r *pb.PullRequest) (
	*pb.PullResponse, error) {

	if err := p.proof(ctx, r.MailboxId); err != nil {
		return nil, err
	}

	if p.pull != nil {
		return p.pull(ctx, r)
	}
	if r.Cursor > p.event.EventSeq {
		if p.emptyStarted != nil {
			p.emptyOnce.Do(func() { close(p.emptyStarted) })
			<-ctx.Done()

			return nil, status.FromContextError(ctx.Err()).Err()
		}

		return &pb.PullResponse{Status: &pb.Status{Ok: true}}, nil
	}

	return feedBatch(p.event), nil
}

// AckUpTo confirms receipt of the same subscriber-key proof on remote ACK.
func (p *feedRPCFixture) AckUpTo(ctx context.Context, r *pb.AckUpToRequest) (
	*pb.AckUpToResponse, error) {

	if err := p.proof(ctx, r.MailboxId); err != nil {
		return nil, err
	}

	if p.ack != nil {
		return p.ack(ctx, r)
	}

	return &pb.AckUpToResponse{Status: &pb.Status{Ok: true}}, nil
}

// startFeedRPCFixture serves a TLS source with real ordinary credentials and R
// proof checks. Policy is installed before any subscriber goroutine starts.
func startFeedRPCFixture(t *testing.T, producer *feedRPCFixture) {
	t.Helper()
	// httptest provides a localhost test certificate without external
	// services.
	certificateServer := httptest.NewTLSServer(nil)
	certificate := certificateServer.TLS.Certificates[0]
	certificateServer.Close()
	certPath := filepath.Join(t.TempDir(), "source.pem")
	require.NoError(
		t,
		os.WriteFile(
			certPath,
			pem.EncodeToMemory(
				&pem.Block{
					Type:  "CERTIFICATE",
					Bytes: certificate.Certificate[0],
				},
			),
			0600,
		),
	)
	mac, err := macaroon.New(
		[]byte("test root"), []byte("client"),
		"producer", macaroon.LatestVersion,
	)
	require.NoError(t, err)
	raw, err := mac.MarshalBinary()
	require.NoError(t, err)
	macPath := filepath.Join(t.TempDir(), "client.macaroon")
	require.NoError(t, os.WriteFile(macPath, raw, 0600))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer(
		grpc.Creds(
			credentials.NewTLS(
				&tls.Config{
					Certificates: []tls.Certificate{
						certificate,
					},
					MinVersion: tls.VersionTLS12,
				},
			),
		),
	)
	producer.expectedMacaroon = hex.EncodeToString(raw)
	pb.RegisterMailboxServiceServer(server, producer)
	pb.RegisterWakeFeedServiceServer(server, producer)
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(listener)
	}()
	t.Cleanup(func() { server.Stop(); assert.NoError(t, <-done) })
	source := producer.source
	source.config.Endpoint = listener.Addr().String()
	source.config.TLSCertFile = certPath
	source.config.MacaroonFile = macPath

}

// TestFeedSubscriberAuthenticatedTransport proves the configured TLS source,
// ordinary macaroon and subscriber proof compose on real RPC calls. A pin
// mismatch is rejected before transmitting static subscriber proof.
func TestFeedSubscriberAuthenticatedTransport(t *testing.T) {
	h, f, event := ingressHarness(t)
	producer := &feedRPCFixture{source: f.source, event: event}
	startFeedRPCFixture(t, producer)
	source := f.source
	subscriber, err := NewFeedSubscriber(t.Context(), h.store, "producer")
	require.NoError(t, err)
	worked, err := subscriber.Step(t.Context())
	require.NoError(t, err)
	require.True(t, worked)
	require.EqualValues(t, 2, producer.reads.Load())
	require.NoError(t, subscriber.Close())
	producer.wrongPin.Store(true)
	_, err = NewFeedSubscriber(t.Context(), h.store, "producer")
	require.ErrorIs(t, err, ErrFeedSource)
	require.EqualValues(
		t, 2, producer.reads.Load(),
		"no R proof sent after identity mismatch",
	)
	source.config.TLSCertFile = ""
	_, err = NewFeedSubscriber(t.Context(), h.store, "producer")
	require.ErrorIs(
		t, err, ErrFeedSource,
		"untrusted TLS is not a valid producer identity",
	)
	require.EqualValues(t, 2, producer.reads.Load())
}
