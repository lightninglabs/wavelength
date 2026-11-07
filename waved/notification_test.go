package waved

import (
	"bytes"
	"context"
	"encoding/base64"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/lib/types"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/notification"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// notificationTestSigner exercises the production daemon's LND signing bridge
// with a real key and the exact raw-message/tag SignMessage contract.
type notificationTestSigner struct {
	lndclient.SignerClient
	key   *btcec.PrivateKey
	calls atomic.Int32
}

// SignMessage applies the supplied BIP-340 tag exactly once, like LND does.
func (s *notificationTestSigner) SignMessage(_ context.Context, message []byte,
	_ keychain.KeyLocator, options ...lndclient.SignMessageOption) ([]byte,
	error) {

	s.calls.Add(1)
	var request signrpc.SignMessageReq
	for _, option := range options {
		option(&request)
	}
	digest := chainhash.TaggedHash(request.Tag, message)
	sig, err := schnorr.Sign(s.key, digest[:])
	if err != nil {
		return nil, err
	}

	return sig.Serialize(), nil
}

// notificationTestEdge verifies the real direct gRPC control exchange.
type notificationTestEdge struct {
	mailboxpb.UnimplementedNotificationServiceServer
	authority *notification.ChallengeAuthority
}

// GetNotificationChallenge issues a real stateless server nonce.
func (e *notificationTestEdge) GetNotificationChallenge(_ context.Context,
	r *mailboxpb.NotificationChallengeRequest) (
	*mailboxpb.NotificationChallenge, error) {

	return e.authority.NewChallenge(r.OwnerPubkey, time.Now())
}

// SetNotificationSubscription verifies the production daemon's complete proof.
func (e *notificationTestEdge) SetNotificationSubscription(_ context.Context,
	r *mailboxpb.NotificationSubscriptionRequest) (
	*mailboxpb.NotificationSubscriptionResponse, error) {

	now := time.Now()
	if err := e.authority.VerifyChallenge(r.Challenge, now); err != nil {
		return nil, err
	}
	if err := notification.VerifyAuthorization(r, now); err != nil {
		return nil, err
	}

	return &mailboxpb.NotificationSubscriptionResponse{
		SubscriptionId: r.Mutation.SubscriptionId,
		ExpiresAtUnix:  r.Mutation.ExpiresAtUnix,
	}, nil
}

// TestNotificationBeforeIngress exercises the actual daemon and wallet-signing
// bridge while wallet-services readiness is unresolved and no ingress exists.
func TestNotificationBeforeIngress(t *testing.T) {
	owner, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	authority, err := notification.NewChallengeAuthority(
		bytes.Repeat(
			[]byte{1}, 32,
		),
		operator.PubKey(),
	)
	require.NoError(t, err)
	listener := bufconn.Listen(1 << 20)
	t.Cleanup(func() { _ = listener.Close() })
	grpcServer := grpc.NewServer()
	mailboxpb.RegisterNotificationServiceServer(
		grpcServer, &notificationTestEdge{
			authority: authority,
		},
	)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- grpcServer.Serve(listener)
	}()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = <-serveDone
	})
	conn, err := grpc.NewClient(
		"passthrough:///notification-edge",
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
		grpc.WithContextDialer(
			func(ctx context.Context, _ string) (net.Conn, error) {
				return listener.DialContext(ctx)
			},
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	signer := &notificationTestSigner{key: owner}
	server := &Server{
		serverConn:          conn,
		walletServicesReady: make(chan struct{}),
		lnd: fn.Some(&lndclient.GrpcLndServices{
			Signer: signer,
		}),
	}
	server.storeClientKeyDesc(
		keychain.KeyDescriptor{
			PubKey: owner.PubKey(),
		},
	)
	server.storeOperatorTerms(
		&types.OperatorTerms{
			PubKey: operator.PubKey(),
		},
	)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	action := mailboxpb.NotificationSubscriptionMutation_ACTION_UPSERT
	mutation := &mailboxpb.NotificationSubscriptionMutation{
		Action:         action,
		SubscriptionId: "route", RelayId: "publisher",
		WakeCapability: base64.RawURLEncoding.EncodeToString(
			bytes.Repeat(
				[]byte{2}, 32,
			),
		),
		ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
	}
	result, err := server.SetNotificationSubscription(ctx, mutation)
	require.NoError(t, err)
	require.Equal(t, mutation.SubscriptionId, result.SubscriptionId)
	// The separately configured swap source uses the same real identity
	// signer, but checks its own pinned issuer before asking the wallet to
	// sign.
	rpcServer := &RPCServer{server: server}
	remote := mailboxpb.NewNotificationServiceClient(conn)
	rpcServer.RegisterSwapNotificationSource(func(ctx context.Context,
		mutation *mailboxpb.NotificationSubscriptionMutation) (
		*mailboxpb.NotificationSubscriptionResponse, error) {

		return rpcServer.AuthorizeNotificationSubscription(
			ctx, remote, operator.PubKey(), mutation,
		)
	})
	result, err = server.SetSwapNotificationSubscription(ctx, mutation)
	require.NoError(t, err)
	require.Equal(t, mutation.SubscriptionId, result.SubscriptionId)
	before := signer.calls.Load()
	wrong, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	rpcServer.RegisterSwapNotificationSource(func(ctx context.Context,
		mutation *mailboxpb.NotificationSubscriptionMutation) (
		*mailboxpb.NotificationSubscriptionResponse, error) {

		return rpcServer.AuthorizeNotificationSubscription(
			ctx, remote, wrong.PubKey(), mutation,
		)
	})
	_, err = server.SetSwapNotificationSubscription(ctx, mutation)
	require.ErrorIs(t, err, notification.ErrInvalidAuthorization)
	require.Equal(t, before, signer.calls.Load())
	rpcServer.RegisterSwapNotificationSource(nil)
	_, err = server.SetSwapNotificationSubscription(ctx, mutation)
	require.ErrorIs(t, err, notification.ErrUnsupported)
	// No bootstrap identity is a readiness error, not an unsupported edge.
	_, err = (&Server{}).SetNotificationSubscription(ctx, mutation)
	require.True(t, waverpc.IsWalletNotReadyError(err))
}
