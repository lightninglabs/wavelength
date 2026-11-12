package notification

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"math"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	pb "github.com/lightninglabs/wavelength/mailbox/pb"
)

const (
	// FeedSignatureTag separates subscriber delegation from push grants and
	// every other wallet-signing operation.
	FeedSignatureTag = "wavelength-wake-feed-grant-v1"

	// FeedRouteSignatureTag binds publisher consent without a wallet
	// identity.
	FeedRouteSignatureTag = "wavelength-wake-feed-route-v1"

	// FeedClassWake permits a hint to reconcile outstanding protocol work.
	FeedClassWake uint32 = 1

	// FeedClassPayment permits an opaque incoming-payment activity hint.
	// It proves neither a completed payment nor a received amount.
	FeedClassPayment uint32 = 2

	// FeedTokenSize is the length of a producer-issued correlation token.
	// A token alone never authorizes a mailbox read or acknowledgement.
	FeedTokenSize = 32
)

// validFeedGrantID requires canonical raw URL-base64 with at least 128 bits.
// Publishers must generate these bytes randomly: IDs share an owner namespace
// across independent publishers and must never be sequential app route names.
func validFeedGrantID(id string) bool {
	if len(id) > 64 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(id)

	return err == nil && len(raw) >= 16 &&
		base64.RawURLEncoding.EncodeToString(raw) == id
}

// validFeedClasses rejects empty masks and unsupported event classes.
func validFeedClasses(classes uint32) bool {
	return classes != 0 && classes&^(FeedClassWake|FeedClassPayment) == 0
}

// validateFeedMutationShape validates canonical fields without a local clock.
func validateFeedMutationShape(m *pb.FeedGrantMutation) error {
	if m == nil || len(m.ProtoReflect().GetUnknown()) != 0 ||
		!validFeedGrantID(m.GrantId) {
		return ErrInvalidAuthorization
	}

	switch m.Action {
	case pb.FeedGrantMutation_ACTION_UPSERT:
		if !canonicalPublicKey(m.SubscriberPubkey) ||
			!validFeedClasses(m.EventClasses) ||
			m.ExpiresAtUnix <= 0 ||
			len(m.SubscriberSignature) != schnorr.SignatureSize {
			return ErrInvalidAuthorization
		}

	case pb.FeedGrantMutation_ACTION_REMOVE:
		if len(m.SubscriberPubkey) != 0 || m.EventClasses != 0 ||
			m.ExpiresAtUnix != 0 ||
			len(m.SubscriberSignature) != 0 {
			return ErrInvalidAuthorization
		}

	default:
		return ErrInvalidAuthorization
	}

	return nil
}

// ValidateFeedMutation applies the producer's maximum delegation lifetime.
// A producer may shorten the requested expiry, but must never extend it.
func ValidateFeedMutation(m *pb.FeedGrantMutation, now time.Time) error {
	if err := validateFeedMutationShape(m); err != nil {
		return err
	}
	if m.Action == pb.FeedGrantMutation_ACTION_UPSERT &&
		(m.ExpiresAtUnix <= now.Unix() ||
			m.ExpiresAtUnix > now.
				Add(MaxSubscriptionLifetime).
				Unix()) {
		return ErrInvalidAuthorization
	}

	return nil
}

// FeedSigningMessage encodes the full v1 delegation in fixed field order.
// Byte/string fields have uint32 big-endian lengths; action and class mask
// are uint32, expiries are int64. Subscriber consent participates; the owner
// signature and protobuf serialization do not. Unknown fields are rejected.
func FeedSigningMessage(r *pb.FeedGrantRequest) ([]byte, error) {
	if r == nil || len(r.ProtoReflect().GetUnknown()) != 0 ||
		!validChallengeShape(r.Challenge) {
		return nil, ErrInvalidAuthorization
	}
	if err := validateFeedMutationShape(r.Mutation); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	writeField := func(value []byte) {
		_ = binary.Write(&buf, binary.BigEndian, uint32(len(value)))
		_, _ = buf.Write(value)
	}
	c, m := r.Challenge, r.Mutation
	writeField(c.OwnerPubkey)
	writeField(c.OperatorPubkey)
	_ = binary.Write(&buf, binary.BigEndian, c.ExpiresAtUnix)
	writeField(c.Token)
	_ = binary.Write(&buf, binary.BigEndian, uint32(m.Action))
	writeField([]byte(m.GrantId))
	writeField(m.SubscriberPubkey)
	_ = binary.Write(&buf, binary.BigEndian, m.EventClasses)
	_ = binary.Write(&buf, binary.BigEndian, m.ExpiresAtUnix)
	writeField(m.SubscriberSignature)

	return buf.Bytes(), nil
}

// VerifyFeedAuthorization verifies freshness and the wallet owner's signature.
// The producer must also verify its challenge MAC, then consume the challenge
// and mutate the grant atomically. Subscriber proof belongs to mailbox auth.
func VerifyFeedAuthorization(r *pb.FeedGrantRequest, now time.Time) error {
	message, err := FeedSigningMessage(r)
	if err != nil {
		return err
	}
	if !validChallenge(r.Challenge, now) {
		return ErrInvalidAuthorization
	}
	if err := ValidateFeedMutation(r.Mutation, now); err != nil {
		return err
	}
	if r.Mutation.Action == pb.FeedGrantMutation_ACTION_UPSERT {
		if err := VerifyFeedRouteConsent(
			r.Challenge.OperatorPubkey, r.Mutation,
		); err != nil {
			return err
		}
	}
	sig, err := schnorr.ParseSignature(r.Signature)
	if err != nil {
		return ErrInvalidAuthorization
	}
	owner, err := btcec.ParsePubKey(r.Challenge.OwnerPubkey)
	if err != nil {
		return ErrInvalidAuthorization
	}
	digest := chainhash.TaggedHash([]byte(FeedSignatureTag), message)
	if !sig.Verify(digest[:], owner) {
		return ErrInvalidAuthorization
	}

	return nil
}

// ValidateFeedEvent rejects ambiguous v1 hints before durable relay admission.
// A caller must separately authenticate the source, resolve the token to a
// locally enrolled route, and enforce the grant's expiry and class mask.
func ValidateFeedEvent(event *pb.WakeFeedEvent) error {
	if event == nil || len(event.ProtoReflect().GetUnknown()) != 0 ||
		len(event.FeedToken) != FeedTokenSize ||
		event.Generation == 0 ||
		event.Generation > math.MaxInt64 ||
		!validFeedClasses(event.EventClasses) ||
		event.ExpiresAtUnix <= 0 || !validFeedGrantID(event.GrantId) {
		return ErrInvalidAuthorization
	}

	if event.EventClasses&FeedClassPayment == 0 {
		if event.PaymentEventId != "" {
			return ErrInvalidAuthorization
		}

		return nil
	}
	correlator, err := hex.DecodeString(event.PaymentEventId)
	if err != nil || len(correlator) != 32 ||
		strings.ToLower(event.PaymentEventId) != event.PaymentEventId {
		return ErrInvalidAuthorization
	}

	return nil
}

// FeedMailboxID reserves a subscriber-key namespace separate from wallet and
// per-payment mailboxes. Existing mailbox authentication proves this key.
func FeedMailboxID(subscriber []byte) (string, error) {
	if !canonicalPublicKey(subscriber) {
		return "", ErrInvalidAuthorization
	}

	return hex.EncodeToString(subscriber) + ":wake-feed", nil
}

// IsFeedMailbox identifies the reserved namespace whose appends never produce
// another wake. Network Send admission must not permit writing this namespace.
func IsFeedMailbox(mailboxID string) bool {
	key, suffix, found := strings.Cut(mailboxID, ":")
	if !found || suffix != "wake-feed" {
		return false
	}
	raw, err := hex.DecodeString(key)
	if err != nil {
		return false
	}
	canonical, err := FeedMailboxID(raw)

	return err == nil && canonical == mailboxID
}

// FeedRouteSigningMessage encodes publisher consent without a wallet identity.
// The producer binding prevents using consent issued for a different source.
func FeedRouteSigningMessage(source, subscriber []byte, grantID string,
	classes uint32, expiry int64) ([]byte, error) {

	if !canonicalPublicKey(source) || !canonicalPublicKey(subscriber) ||
		!validFeedGrantID(grantID) ||
		!validFeedClasses(classes) || expiry <= 0 {
		return nil, ErrInvalidAuthorization
	}
	var buf bytes.Buffer
	for _, field := range [][]byte{source, subscriber, []byte(grantID)} {
		_ = binary.Write(&buf, binary.BigEndian, uint32(len(field)))
		_, _ = buf.Write(field)
	}
	_ = binary.Write(&buf, binary.BigEndian, classes)
	_ = binary.Write(&buf, binary.BigEndian, expiry)

	return buf.Bytes(), nil
}

// VerifyFeedRouteConsent proves the subscriber admitted this exact route.
// The producer must separately retain a unique (subscriber, grant ID) owner
// binding until expiry, so one consent cannot allocate many wallet grants.
func VerifyFeedRouteConsent(source []byte, m *pb.FeedGrantMutation) error {
	if err := validateFeedMutationShape(m); err != nil {
		return err
	}
	if m.Action != pb.FeedGrantMutation_ACTION_UPSERT {
		return ErrInvalidAuthorization
	}
	message, err := FeedRouteSigningMessage(
		source, m.SubscriberPubkey, m.GrantId, m.EventClasses,
		m.ExpiresAtUnix,
	)
	if err != nil {
		return err
	}
	key, err := btcec.ParsePubKey(m.SubscriberPubkey)
	if err != nil {
		return ErrInvalidAuthorization
	}
	sig, err := schnorr.ParseSignature(m.SubscriberSignature)
	if err != nil {
		return ErrInvalidAuthorization
	}
	digest := chainhash.TaggedHash([]byte(FeedRouteSignatureTag), message)
	if !sig.Verify(digest[:], key) {
		return ErrInvalidAuthorization
	}

	return nil
}
