package notification

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// feedFixture signs a real subscriber delegation with independent identities.
func feedFixture(t *testing.T) (*ChallengeAuthority, *pb.FeedGrantRequest,
	*btcec.PrivateKey, time.Time) {

	t.Helper()
	authority, old, owner, now := authFixture(t)
	subscriber, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	request := &pb.FeedGrantRequest{
		Challenge: old.Challenge,
		Mutation: &pb.FeedGrantMutation{
			Action:  pb.FeedGrantMutation_ACTION_UPSERT,
			GrantId: "AAECAwQFBgcICQoLDA0ODw",
			SubscriberPubkey: subscriber.
				PubKey().
				SerializeCompressed(),
			EventClasses:  FeedClassWake | FeedClassPayment,
			ExpiresAtUnix: now.Add(time.Hour).Unix(),
		},
	}
	routeMessage, err := FeedRouteSigningMessage(
		request.Challenge.OperatorPubkey,
		request.Mutation.SubscriberPubkey, request.Mutation.GrantId,
		request.Mutation.EventClasses, request.Mutation.ExpiresAtUnix,
	)
	require.NoError(t, err)
	routeDigest := chainhash.TaggedHash(
		[]byte(FeedRouteSignatureTag), routeMessage,
	)
	routeSignature, err := schnorr.Sign(subscriber, routeDigest[:])
	require.NoError(t, err)
	request.Mutation.SubscriberSignature = routeSignature.Serialize()
	signFeed(t, request, owner, FeedSignatureTag)

	return authority, request, owner, now
}

// signFeed signs the exact transcript under the supplied domain.
func signFeed(t *testing.T, request *pb.FeedGrantRequest, key *btcec.PrivateKey,
	tag string) {

	t.Helper()
	message, err := FeedSigningMessage(request)
	require.NoError(t, err)
	digest := chainhash.TaggedHash([]byte(tag), message)
	sig, err := schnorr.Sign(key, digest[:])
	require.NoError(t, err)
	request.Signature = sig.Serialize()
}

// TestFeedProofBinding rejects changes to every signed authority field.
func TestFeedProofBinding(t *testing.T) {
	authority, request, _, now := feedFixture(t)
	require.NoError(t, authority.VerifyChallenge(request.Challenge, now))
	require.NoError(t, VerifyFeedAuthorization(request, now))
	changes := map[string]func(*pb.FeedGrantRequest){
		"owner": func(r *pb.FeedGrantRequest) {
			r.Challenge.OwnerPubkey = r.Mutation.SubscriberPubkey
		},
		"source": func(r *pb.FeedGrantRequest) {
			r.Challenge.OperatorPubkey = r.Mutation.SubscriberPubkey
		},
		"nonce": func(r *pb.FeedGrantRequest) {
			r.Challenge.Token[0] ^= 1
		},
		"challenge expiry": func(r *pb.FeedGrantRequest) {
			r.Challenge.ExpiresAtUnix--
		},
		"grant id": func(r *pb.FeedGrantRequest) {
			r.Mutation.GrantId = "EBESExQVFhcYGRobHB0eHw"
		},
		"subscriber": func(r *pb.FeedGrantRequest) {
			r.Mutation.SubscriberPubkey = r.Challenge.OwnerPubkey
		},
		"classes": func(r *pb.FeedGrantRequest) {
			r.Mutation.EventClasses = FeedClassWake
		},
		"grant expiry": func(r *pb.FeedGrantRequest) {
			r.Mutation.ExpiresAtUnix--
		},
		"action": func(r *pb.FeedGrantRequest) {
			r.Mutation = &pb.FeedGrantMutation{
				Action:  pb.FeedGrantMutation_ACTION_REMOVE,
				GrantId: r.Mutation.GrantId,
			}
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			changed := new(pb.FeedGrantRequest)
			proto.Merge(changed, request)
			change(changed)
			require.ErrorIs(
				t, VerifyFeedAuthorization(changed, now),
				ErrInvalidAuthorization,
			)
		})
	}
}

// TestFeedAuthorizationBoundaries covers version, expiry and domain separation.
func TestFeedAuthorizationBoundaries(t *testing.T) {
	_, request, owner, now := feedFixture(t)
	for _, tag := range []string{SignatureTag, "mailbox-auth"} {
		signFeed(t, request, owner, tag)
		require.ErrorIs(
			t, VerifyFeedAuthorization(request, now),
			ErrInvalidAuthorization,
		)
	}
	signFeed(t, request, owner, FeedSignatureTag)
	require.ErrorIs(
		t,
		VerifyFeedAuthorization(
			request, now.Add(ChallengeLifetime),
		),
		ErrInvalidAuthorization,
	)
	for _, message := range []proto.Message{
		request, request.Challenge, request.Mutation,
	} {
		message.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
		_, err := FeedSigningMessage(request)
		require.ErrorIs(t, err, ErrInvalidAuthorization)
		message.ProtoReflect().SetUnknown(nil)
	}
	request.Mutation.EventClasses = 4
	_, err := FeedSigningMessage(request)
	require.ErrorIs(t, err, ErrInvalidAuthorization)
	request.Mutation.EventClasses = FeedClassWake
	request.Mutation.ExpiresAtUnix = now.Add(MaxSubscriptionLifetime).Unix()
	require.NoError(t, ValidateFeedMutation(request.Mutation, now))
	request.Mutation.ExpiresAtUnix++
	require.ErrorIs(
		t, ValidateFeedMutation(request.Mutation, now),
		ErrInvalidAuthorization,
	)
	request.Mutation = &pb.FeedGrantMutation{
		Action:  pb.FeedGrantMutation_ACTION_REMOVE,
		GrantId: "AAECAwQFBgcICQoLDA0ODw",
	}
	signFeed(t, request, owner, FeedSignatureTag)
	require.NoError(t, VerifyFeedAuthorization(request, now))
	request.Mutation.EventClasses = FeedClassWake
	_, err = FeedSigningMessage(request)
	require.ErrorIs(t, err, ErrInvalidAuthorization)
}

// feedRPC checks authorization and lets the client tests corrupt edge replies.
type feedRPC struct {
	authority   *ChallengeAuthority
	now         time.Time
	challenge   *pb.NotificationChallenge
	response    func(*pb.FeedGrantResponse)
	unsupported bool
	requests    int
}

// GetFeedChallenge issues a producer-bound stateless challenge.
func (f *feedRPC) GetFeedChallenge(_ context.Context,
	r *pb.NotificationChallengeRequest, _ ...grpc.CallOption) (
	*pb.NotificationChallenge, error) {

	if f.unsupported {
		return nil, status.Error(codes.Unimplemented, "unsupported")
	}
	if f.challenge != nil {
		return f.challenge, nil
	}

	return f.authority.NewChallenge(r.OwnerPubkey, f.now)
}

// SetFeedGrant verifies the signed request before returning a correlation
// token.
func (f *feedRPC) SetFeedGrant(_ context.Context, r *pb.FeedGrantRequest,
	_ ...grpc.CallOption) (*pb.FeedGrantResponse, error) {

	f.requests++
	if err := f.authority.VerifyChallenge(r.Challenge, f.now); err != nil {
		return nil, err
	}
	if err := VerifyFeedAuthorization(r, f.now); err != nil {
		return nil, err
	}
	response := &pb.FeedGrantResponse{GrantId: r.Mutation.GrantId}
	if r.Mutation.Action == pb.FeedGrantMutation_ACTION_UPSERT {
		response.FeedToken = bytes.Repeat([]byte{8}, FeedTokenSize)
		response.ExpiresAtUnix = r.Mutation.ExpiresAtUnix
	}
	if f.response != nil {
		f.response(response)
	}

	return response, nil
}

// TestFeedClientSourceBinding rejects a source substitution before signing.
func TestFeedClientSourceBinding(t *testing.T) {
	authority, request, owner, now := feedFixture(t)
	rpc := &feedRPC{authority: authority, now: now}
	signs := 0
	signer := func(_ context.Context, message, tag []byte) (
		*schnorr.Signature, error) {

		signs++
		require.Equal(t, FeedSignatureTag, string(tag))
		digest := chainhash.TaggedHash(tag, message)

		return schnorr.Sign(owner, digest[:])
	}
	source := mustPublicKey(t, request.Challenge.OperatorPubkey)
	response, err := SetFeedGrant(
		t.Context(), rpc, owner.PubKey(), source, request.Mutation,
		signer,
	)
	require.NoError(t, err)
	require.Len(t, response.FeedToken, FeedTokenSize)
	remove := &pb.FeedGrantMutation{
		Action:  pb.FeedGrantMutation_ACTION_REMOVE,
		GrantId: request.Mutation.GrantId,
	}
	response, err = SetFeedGrant(
		t.Context(), rpc, owner.PubKey(), source, remove, signer,
	)
	require.NoError(t, err)
	require.Empty(t, response.FeedToken)
	rpc.challenge = new(pb.NotificationChallenge)
	proto.Merge(rpc.challenge, request.Challenge)
	rpc.challenge.OperatorPubkey = request.Mutation.SubscriberPubkey
	_, err = SetFeedGrant(
		t.Context(), rpc, owner.PubKey(), source, request.Mutation,
		signer,
	)
	require.ErrorIs(t, err, ErrInvalidAuthorization)
	require.Equal(t, 2, signs)
	require.Equal(t, 2, rpc.requests)
	rpc.unsupported = true
	_, err = SetFeedGrant(
		t.Context(), rpc, owner.PubKey(), source, request.Mutation,
		signer,
	)
	require.ErrorIs(t, err, ErrUnsupported)
}

// TestFeedClientResponseBinding rejects an unrelated or overlong delegation.
func TestFeedClientResponseBinding(t *testing.T) {
	authority, request, owner, now := feedFixture(t)
	changes := map[string]func(*pb.FeedGrantResponse){
		"other grant": func(r *pb.FeedGrantResponse) {
			r.GrantId = "EBESExQVFhcYGRobHB0eHw"
		},
		"missing token": func(r *pb.FeedGrantResponse) {
			r.FeedToken = nil
		},
		"extended expiry": func(r *pb.FeedGrantResponse) {
			r.ExpiresAtUnix++
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			rpc := &feedRPC{
				authority: authority,
				now:       now,
				response:  change,
			}
			signer := func(_ context.Context, message, tag []byte) (
				*schnorr.Signature, error) {

				digest := chainhash.TaggedHash(tag, message)

				return schnorr.Sign(owner, digest[:])
			}
			_, err := SetFeedGrant(
				t.Context(), rpc, owner.PubKey(),
				mustPublicKey(
					t, request.Challenge.OperatorPubkey,
				),
				request.Mutation,
				signer,
			)
			require.ErrorIs(t, err, ErrInvalidAuthorization)
		})
	}
}

// TestFeedEventClassBinding rejects a correlator without payment authority.
func TestFeedEventClassBinding(t *testing.T) {
	event := &pb.WakeFeedEvent{
		GrantId:      "AAECAwQFBgcICQoLDA0ODw",
		FeedToken:    bytes.Repeat([]byte{9}, FeedTokenSize),
		Generation:   1,
		EventClasses: FeedClassWake, ExpiresAtUnix: 100,
	}
	require.NoError(t, ValidateFeedEvent(event))
	event.PaymentEventId = strings.Repeat("ab", 32)
	require.ErrorIs(t, ValidateFeedEvent(event), ErrInvalidAuthorization)
	event.EventClasses |= FeedClassPayment
	require.NoError(t, ValidateFeedEvent(event))
	event.PaymentEventId = strings.ToUpper(event.PaymentEventId)
	require.ErrorIs(t, ValidateFeedEvent(event), ErrInvalidAuthorization)
	event.PaymentEventId = strings.Repeat("ab", 32)
	event.Generation = 1 << 63
	require.ErrorIs(t, ValidateFeedEvent(event), ErrInvalidAuthorization)
}

// TestFeedMailboxNamespace keeps subscriber traffic distinct from wallet work.
func TestFeedMailboxNamespace(t *testing.T) {
	_, request, _, _ := feedFixture(t)
	mailboxID, err := FeedMailboxID(request.Mutation.SubscriberPubkey)
	require.NoError(t, err)
	require.True(t, IsFeedMailbox(mailboxID))
	require.False(
		t,
		IsFeedMailbox(
			strings.TrimSuffix(mailboxID, ":wake-feed"),
		),
	)
	require.False(t, IsFeedMailbox(mailboxID+":extra"))
	require.False(t, IsFeedMailbox("invalid:wake-feed"))
	_, err = FeedMailboxID(nil)
	require.ErrorIs(t, err, ErrInvalidAuthorization)
}

// TestFeedSubscriberConsent rejects wallet-signed grants without publisher
// consent, including a valid consent transplanted to another route or source.
func TestFeedSubscriberConsent(t *testing.T) {
	_, request, owner, now := feedFixture(t)
	changes := map[string]func(*pb.FeedGrantRequest){
		"subscriber namespace": func(r *pb.FeedGrantRequest) {
			r.Mutation.SubscriberPubkey[0] ^= 1
		},
		"signature": func(r *pb.FeedGrantRequest) {
			r.Mutation.SubscriberSignature[0] ^= 1
		},
		"route": func(r *pb.FeedGrantRequest) {
			r.Mutation.GrantId = "EBESExQVFhcYGRobHB0eHw"
		},
		"source": func(r *pb.FeedGrantRequest) {
			r.Challenge.OperatorPubkey = r.Challenge.OwnerPubkey
		},
		"classes": func(r *pb.FeedGrantRequest) {
			r.Mutation.EventClasses = FeedClassWake
		},
		"expiry": func(r *pb.FeedGrantRequest) {
			r.Mutation.ExpiresAtUnix--
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			changed := new(pb.FeedGrantRequest)
			proto.Merge(changed, request)
			change(changed)
			signFeed(t, changed, owner, FeedSignatureTag)
			require.ErrorIs(
				t, VerifyFeedAuthorization(changed, now),
				ErrInvalidAuthorization,
			)
		})
	}
}

// TestFeedGrantIdentifierShape rejects short/local names that independent
// publishers could accidentally collide in the same wallet-owner namespace.
func TestFeedGrantIdentifierShape(t *testing.T) {
	_, request, _, _ := feedFixture(t)
	for _, id := range []string{
		"1",
		"route",
		"publisher.route",
		"AAECAwQFBgcICQoLDA0ODw==",
		strings.Repeat("a", 65),
	} {
		request.Mutation.GrantId = id
		_, err := FeedSigningMessage(request)
		require.ErrorIs(t, err, ErrInvalidAuthorization)
	}
	request.Mutation.GrantId = "AAECAwQFBgcICQoLDA0ODw"
	_, err := FeedSigningMessage(request)
	require.NoError(t, err)
}
