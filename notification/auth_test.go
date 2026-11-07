package notification

import (
	"bytes"
	"context"
	"encoding/base64"
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

// Short aliases keep test cases focused on changes to the wire contract.
const (
	testUpsert = pb.NotificationSubscriptionMutation_ACTION_UPSERT
	testRemove = pb.NotificationSubscriptionMutation_ACTION_REMOVE
)

// authFixture creates real mailbox/operator identities and a signed request.
func authFixture(t *testing.T) (*ChallengeAuthority,
	*pb.NotificationSubscriptionRequest, *btcec.PrivateKey, time.Time) {

	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	owner, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	authority, err := NewChallengeAuthority(
		bytes.Repeat(
			[]byte{1}, 32,
		),
		operator.PubKey(),
	)
	require.NoError(t, err)
	challenge, err := authority.NewChallenge(
		owner.PubKey().SerializeCompressed(), now,
	)
	require.NoError(t, err)
	request := &pb.NotificationSubscriptionRequest{
		Challenge: challenge,
		Mutation: &pb.NotificationSubscriptionMutation{
			Action:         testUpsert,
			SubscriptionId: "relay-subscription",
			RelayId:        "publisher-relay",
			WakeCapability: base64.RawURLEncoding.EncodeToString(
				bytes.Repeat(
					[]byte{2}, 32,
				),
			),
			AlertCapability: base64.RawURLEncoding.EncodeToString(
				bytes.Repeat(
					[]byte{3}, 32,
				),
			),
			ExpiresAtUnix: now.Add(time.Hour).Unix(),
		},
	}
	message, err := SigningMessage(request)
	require.NoError(t, err)
	digest := chainhash.TaggedHash([]byte(SignatureTag), message)
	sig, err := schnorr.Sign(owner, digest[:])
	require.NoError(t, err)
	request.Signature = sig.Serialize()

	return authority, request, owner, now
}

// TestNotificationProofBinding changes every signed routing/authority field.
// A captured static ownership proof cannot authorize a changed subscription.
func TestNotificationProofBinding(t *testing.T) {
	authority, request, _, now := authFixture(t)
	require.NoError(t, authority.VerifyChallenge(request.Challenge, now))
	require.NoError(t, VerifyAuthorization(request, now))
	mutations := map[string]func(*pb.NotificationSubscriptionRequest){
		"owner": func(r *pb.NotificationSubscriptionRequest) {
			r.Challenge.OwnerPubkey = r.Challenge.OperatorPubkey
		},
		"operator": func(r *pb.NotificationSubscriptionRequest) {
			r.Challenge.OperatorPubkey = r.Challenge.OwnerPubkey
		},
		"challenge-expiry": func(
			r *pb.NotificationSubscriptionRequest) {

			r.Challenge.ExpiresAtUnix--
		},
		"nonce": func(r *pb.NotificationSubscriptionRequest) {
			r.Challenge.Token[0] ^= 1
		},
		"subscription": func(r *pb.NotificationSubscriptionRequest) {
			r.Mutation.SubscriptionId = "other-subscription"
		},
		"relay": func(r *pb.NotificationSubscriptionRequest) {
			r.Mutation.RelayId = "other-relay"
		},
		"wake": func(r *pb.NotificationSubscriptionRequest) {
			r.Mutation.WakeCapability = r.Mutation.AlertCapability
		},
		"alert": func(r *pb.NotificationSubscriptionRequest) {
			r.Mutation.AlertCapability = ""
		},
		"expiry": func(r *pb.NotificationSubscriptionRequest) {
			r.Mutation.ExpiresAtUnix--
		},
		"remove": func(r *pb.NotificationSubscriptionRequest) {
			r.Mutation = &pb.NotificationSubscriptionMutation{
				Action:         testRemove,
				SubscriptionId: r.Mutation.SubscriptionId,
			}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			cloned := new(pb.NotificationSubscriptionRequest)
			proto.Merge(cloned, request)
			mutate(cloned)
			require.ErrorIs(
				t, VerifyAuthorization(cloned, now),
				ErrInvalidAuthorization,
			)
		})
	}
}

// TestNotificationChallengeBoundaries verifies expiry, MAC/key persistence and
// cross-operator rejection without allocating anonymous replay records.
func TestNotificationChallengeBoundaries(t *testing.T) {
	authority, request, _, now := authFixture(t)
	c := request.Challenge
	restored, err := NewChallengeAuthority(
		authority.key[:], mustPublicKey(t, c.OperatorPubkey),
	)
	require.NoError(t, err)
	require.NoError(t, restored.VerifyChallenge(c, now))
	wrong, err := NewChallengeAuthority(
		bytes.Repeat(
			[]byte{9}, 32,
		),
		mustPublicKey(t, c.OperatorPubkey),
	)
	require.NoError(t, err)
	require.ErrorIs(
		t, wrong.VerifyChallenge(c, now), ErrInvalidAuthorization,
	)
	require.ErrorIs(
		t,
		authority.VerifyChallenge(
			c, now.Add(ChallengeLifetime),
		),
		ErrInvalidAuthorization,
	)
	require.ErrorIs(
		t,
		VerifyAuthorization(
			request, now.Add(ChallengeLifetime),
		),
		ErrInvalidAuthorization,
	)
	c.Token[0] ^= 1
	require.ErrorIs(
		t, authority.VerifyChallenge(c, now), ErrInvalidAuthorization,
	)
	c.Token[0] ^= 1
	c.OperatorPubkey = c.OwnerPubkey
	require.ErrorIs(
		t, authority.VerifyChallenge(c, now), ErrInvalidAuthorization,
	)
}

// mustPublicKey parses a fixture identity before constructing a verifier.
func mustPublicKey(t *testing.T, raw []byte) *btcec.PublicKey {
	t.Helper()
	key, err := btcec.ParsePubKey(raw)
	require.NoError(t, err)

	return key
}

// subscriptionRPC captures the signed mutation and verifies it like an edge.
// Production replay consumption belongs to the operator database transaction.
type subscriptionRPC struct {
	authority   *ChallengeAuthority
	now         time.Time
	challenge   *pb.NotificationChallenge
	requests    int
	unsupported bool
}

// GetNotificationChallenge returns a controlled challenge or an older-edge
// error.
func (s *subscriptionRPC) GetNotificationChallenge(_ context.Context,
	r *pb.NotificationChallengeRequest, _ ...grpc.CallOption) (
	*pb.NotificationChallenge, error) {

	if s.unsupported {
		return nil, status.Error(codes.Unimplemented, "unsupported")
	}
	if s.challenge != nil {
		return s.challenge, nil
	}

	return s.authority.NewChallenge(r.OwnerPubkey, s.now)
}

// SetNotificationSubscription verifies proof binding before accepting the
// route.
func (s *subscriptionRPC) SetNotificationSubscription(_ context.Context,
	r *pb.NotificationSubscriptionRequest, _ ...grpc.CallOption) (
	*pb.NotificationSubscriptionResponse, error) {

	s.requests++
	if err := s.authority.VerifyChallenge(r.Challenge, s.now); err != nil {
		return nil, err
	}
	if err := VerifyAuthorization(r, s.now); err != nil {
		return nil, err
	}

	return &pb.NotificationSubscriptionResponse{
		SubscriptionId: r.Mutation.SubscriptionId,
		ExpiresAtUnix:  r.Mutation.ExpiresAtUnix,
	}, nil
}

// TestNotificationClient verifies the direct control RPC path, negotiated-key
// binding and older-operator fallback without running mailbox ingress.
func TestNotificationClient(t *testing.T) {
	authority, request, owner, now := authFixture(t)
	rpc := &subscriptionRPC{authority: authority, now: now}
	signs := 0
	sign := func(_ context.Context, message, tag []byte) (
		*schnorr.Signature, error) {

		signs++
		digest := chainhash.TaggedHash(tag, message)

		return schnorr.Sign(owner, digest[:])
	}
	operator := mustPublicKey(t, request.Challenge.OperatorPubkey)
	result, err := SetSubscription(
		t.Context(), rpc, owner.PubKey(), operator, request.Mutation,
		sign,
	)
	require.NoError(t, err)
	require.Equal(t, request.Mutation.SubscriptionId, result.SubscriptionId)
	require.Equal(t, 1, signs)
	require.Equal(t, 1, rpc.requests)
	remove := &pb.NotificationSubscriptionMutation{
		Action:         testRemove,
		SubscriptionId: request.Mutation.SubscriptionId,
	}
	removed, err := SetSubscription(
		t.Context(), rpc, owner.PubKey(), operator, remove, sign,
	)
	require.NoError(t, err)
	require.Zero(t, removed.ExpiresAtUnix)
	require.Equal(t, 2, signs)
	require.Equal(t, 2, rpc.requests)
	rpc.challenge = request.Challenge
	rpc.challenge.OperatorPubkey = owner.PubKey().SerializeCompressed()
	_, err = SetSubscription(
		t.Context(), rpc, owner.PubKey(), operator, request.Mutation,
		sign,
	)
	require.ErrorIs(t, err, ErrInvalidAuthorization)
	require.Equal(
		t, 2, signs, "wrong operator must be rejected before signing",
	)
	require.Equal(t, 2, rpc.requests)
	rpc.unsupported = true
	_, err = SetSubscription(
		t.Context(), rpc, owner.PubKey(), operator, request.Mutation,
		sign,
	)
	require.ErrorIs(t, err, ErrUnsupported)
	require.Equal(t, 2, signs)
}

// TestNotificationDomainAndVersion rejects static mailbox-domain signatures
// and unknown v1 fields instead of signing an ambiguous future interpretation.
func TestNotificationDomainAndVersion(t *testing.T) {
	_, request, owner, now := authFixture(t)
	message, err := SigningMessage(request)
	require.NoError(t, err)
	digest := chainhash.TaggedHash([]byte("mailbox-auth"), message)
	sig, err := schnorr.Sign(owner, digest[:])
	require.NoError(t, err)
	request.Signature = sig.Serialize()
	require.ErrorIs(
		t, VerifyAuthorization(request, now), ErrInvalidAuthorization,
	)
	request.Mutation.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	_, err = SigningMessage(request)
	require.ErrorIs(t, err, ErrInvalidAuthorization)
	request.Mutation.ProtoReflect().SetUnknown(nil)
	request.Mutation.RelayId = "https://untrusted.example/"
	_, err = SigningMessage(request)
	require.ErrorIs(t, err, ErrInvalidAuthorization)
	request.Mutation.RelayId = "publisher-relay"
	request.Mutation.ExpiresAtUnix = now.
		Add(MaxSubscriptionLifetime + time.Hour).
		Unix()
	err = ValidateMutation(request.Mutation, now)
	require.ErrorIs(t, err, ErrInvalidAuthorization)
}

// TestNotificationClientClockSkew proves the client signs under the issuer's
// time policy even when its own wall clock lags the operator and relay.
func TestNotificationClientClockSkew(t *testing.T) {
	authority, request, owner, now := authFixture(t)
	rpc := &subscriptionRPC{authority: authority, now: now.Add(time.Minute)}
	request.Mutation.ExpiresAtUnix = rpc.now.
		Add(MaxSubscriptionLifetime).
		Unix()
	sign := func(_ context.Context, message, tag []byte) (
		*schnorr.Signature, error) {

		digest := chainhash.TaggedHash(tag, message)

		return schnorr.Sign(owner, digest[:])
	}
	operator := mustPublicKey(t, request.Challenge.OperatorPubkey)
	result, err := SetSubscription(
		t.Context(), rpc, owner.PubKey(), operator, request.Mutation,
		sign,
	)
	require.NoError(t, err)
	require.Equal(t, request.Mutation.ExpiresAtUnix, result.ExpiresAtUnix)
	// Server policy permits bounded relay skew, never an arbitrary horizon.
	request.Mutation.ExpiresAtUnix = now.
		Add(MaxSubscriptionLifetime + time.Minute).
		Unix()
	require.NoError(t, ValidateMutation(request.Mutation, now))
	request.Mutation.ExpiresAtUnix = now.
		Add(MaxSubscriptionLifetime + 6*time.Minute).
		Unix()
	require.ErrorIs(
		t, ValidateMutation(request.Mutation, now),
		ErrInvalidAuthorization,
	)
}
