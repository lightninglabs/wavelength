package notifyrelay

import (
	"bytes"
	"context"
	"encoding/hex"
	"time"

	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/rpcauth"
	"github.com/lightninglabs/wavelength/serverconn"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// FeedSubscriber owns one authenticated producer connection. The daemon owns
// its Step loop and joins that loop before Close and database shutdown.
// It holds a subscriber key, never wallet-signing authority.
type FeedSubscriber struct {
	ingress    *feedIngress
	connection *grpc.ClientConn
}

// NewFeedSubscriber connects only to publisher configuration over authenticated
// TLS. It checks the logical source identity before transmitting the static R
// mailbox proof. That identity is supplied over TLS, not a new server
// signature.
func NewFeedSubscriber(ctx context.Context, store *SQLStore,
	sourceID string) (*FeedSubscriber, error) {

	if store == nil {
		return nil, ErrFeedSource
	}
	source, ok := store.sources[sourceID]
	if !ok {
		return nil, ErrFeedSource
	}
	credentials, err := rpcauth.ClientTLSCredentials(
		source.config.TLSCertFile,
	)
	if err != nil {
		return nil, ErrFeedSource
	}
	options := []grpc.DialOption{grpc.WithTransportCredentials(credentials)}
	if source.config.MacaroonFile != "" {
		option, err := rpcauth.DialOptionFromFile(
			source.config.MacaroonFile,
		)
		if err != nil {
			return nil, ErrFeedSource
		}
		options = append(options, option)
	}
	connection, err := grpc.NewClient(source.config.Endpoint, options...)
	if err != nil {
		return nil, ErrFeedSource
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	feedRPC := pb.NewWakeFeedServiceClient(connection)
	challenge, err := feedRPC.GetFeedChallenge(
		probeCtx, &pb.NotificationChallengeRequest{
			OwnerPubkey: source.subscriber.
				PubKey().
				SerializeCompressed(),
		},
	)
	cancel()
	if err != nil || challenge == nil ||
		!bytes.Equal(
			challenge.OperatorPubkey,
			source.public.SerializeCompressed(),
		) ||
		!bytes.Equal(
			challenge.OwnerPubkey,
			source.subscriber.PubKey().SerializeCompressed(),
		) {

		_ = connection.Close()

		return nil, ErrFeedSource
	}
	signature, err := serverconn.SignMailboxAuth(
		source.subscriber, source.mailboxID,
	)
	if err != nil {
		_ = connection.Close()

		return nil, ErrFeedSource
	}
	transport := &authenticatedFeedMailbox{
		client:    pb.NewMailboxServiceClient(connection),
		signature: hex.EncodeToString(signature.Serialize()),
	}

	return &FeedSubscriber{connection: connection, ingress: &feedIngress{
		store: store, source: source, mailbox: transport,
	}}, nil
}

// Step commits one bounded source prefix and its local push obligations. A
// false result means no progress this step, not that the wallet is idle.
func (s *FeedSubscriber) Step(ctx context.Context) (bool, error) {
	return s.ingress.step(
		ctx,
	)
}

// Close releases the producer connection after its caller has joined Step.
func (s *FeedSubscriber) Close() error { return s.connection.Close() }

// authenticatedFeedMailbox attaches the reserved-mailbox proof without erasing
// other outgoing metadata or the connection's ordinary RPC credentials.
type authenticatedFeedMailbox struct {
	client    pb.MailboxServiceClient
	signature string
}

// Pull proves only the subscriber's configured read-only mailbox key.
func (m *authenticatedFeedMailbox) Pull(ctx context.Context, r *pb.PullRequest,
	options ...grpc.CallOption) (*pb.PullResponse, error) {

	ctx = metadata.AppendToOutgoingContext(
		ctx, serverconn.AuthHeaderKey, m.signature,
	)

	return m.client.Pull(ctx, r, options...)
}

// AckUpTo uses the same R proof after the local checkpoint transaction commits.
func (m *authenticatedFeedMailbox) AckUpTo(ctx context.Context,
	r *pb.AckUpToRequest, options ...grpc.CallOption) (*pb.AckUpToResponse,
	error) {

	ctx = metadata.AppendToOutgoingContext(
		ctx, serverconn.AuthHeaderKey, m.signature,
	)

	return m.client.AckUpTo(ctx, r, options...)
}
