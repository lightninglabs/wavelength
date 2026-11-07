// Package notification binds fixed-relay subscriptions to a fresh request
// authorized by the mailbox owner's identity key. It owns no database or
// delivery worker. The operator consumes a verified challenge atomically with
// its subscription mutation; transport authentication alone is insufficient.
package notification

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	pb "github.com/lightninglabs/wavelength/mailbox/pb"
)

const (
	// SignatureTag separates full subscription authorization from static
	// mailbox ownership proofs and every wallet signing protocol.
	SignatureTag = "wavelength-notification-subscription-v1"

	// ChallengeLifetime bounds signed management authority before
	// consumption.
	ChallengeLifetime = 5 * time.Minute

	// MaxSubscriptionLifetime never extends a relay's 30-day capability.
	MaxSubscriptionLifetime = 30 * 24 * time.Hour

	// subscriptionClockSkew permits ordinary operator/relay clock
	// differences. The server still caps stored expiry to the verified
	// relay grant.
	subscriptionClockSkew = 5 * time.Minute
)

// ErrInvalidAuthorization hides proof, capability and mailbox identifiers from
// diagnostics. Callers must never log the full signed request.
var ErrInvalidAuthorization = errors.New("invalid notification authorization")

// ChallengeAuthority issues stateless server nonces. Its persistent 32-byte
// secret belongs to one operator and must survive restart. Successful proof
// consumption still needs an atomic database replay record, keyed by
// ChallengeID.
type ChallengeAuthority struct {
	key      [32]byte
	operator []byte
}

// NewChallengeAuthority copies a persisted server secret and canonical operator
// identity. It does not create or persist a new key implicitly at restart.
func NewChallengeAuthority(key []byte,
	operator *btcec.PublicKey) (*ChallengeAuthority, error) {

	if len(key) != 32 || operator == nil {
		return nil, ErrInvalidAuthorization
	}
	a := &ChallengeAuthority{operator: operator.SerializeCompressed()}
	copy(a.key[:], key)

	return a, nil
}

// NewChallenge returns a five-minute nonce without allocating persistent state.
func (a *ChallengeAuthority) NewChallenge(owner []byte, now time.Time) (
	*pb.NotificationChallenge, error) {

	if !canonicalPublicKey(owner) {
		return nil, ErrInvalidAuthorization
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	challenge := &pb.NotificationChallenge{
		OwnerPubkey: bytes.Clone(owner), OperatorPubkey: bytes.Clone(
			a.operator,
		),
		ExpiresAtUnix: now.Add(ChallengeLifetime).Unix(),
	}
	challenge.Token = nonce
	challenge.Token = append(
		challenge.Token, a.challengeMAC(challenge, nonce)...,
	)

	return challenge, nil
}

// challengeMAC authenticates nonce, subject, operator and expiry together.
func (a *ChallengeAuthority) challengeMAC(c *pb.NotificationChallenge,
	nonce []byte) []byte {

	mac := hmac.New(sha256.New, a.key[:])
	_, _ = mac.Write([]byte("wavelength-notification-challenge-v1\x00"))
	_, _ = mac.Write(c.OwnerPubkey)
	_, _ = mac.Write(c.OperatorPubkey)
	_ = binary.Write(mac, binary.BigEndian, c.ExpiresAtUnix)
	_, _ = mac.Write(nonce)

	return mac.Sum(nil)
}

// VerifyChallenge validates this operator's nonce without consuming it. The
// caller verifies the owner signature before inserting any replay record, then
// inserts that record in the same transaction as its subscription change.
func (a *ChallengeAuthority) VerifyChallenge(c *pb.NotificationChallenge,
	now time.Time) error {

	if !validChallenge(c, now) ||
		!bytes.Equal(c.OperatorPubkey, a.operator) {
		return ErrInvalidAuthorization
	}
	if !hmac.Equal(c.Token[32:], a.challengeMAC(c, c.Token[:32])) {
		return ErrInvalidAuthorization
	}

	return nil
}

// ChallengeID hashes the authenticated token for the operator's replay key.
// Call only after verifying both the challenge MAC and full-request signature.
func ChallengeID(c *pb.NotificationChallenge) [32]byte {
	return sha256.Sum256(c.GetToken())
}

// canonicalPublicKey rejects uncompressed or noncanonical mailbox identities.
func canonicalPublicKey(raw []byte) bool {
	if len(raw) != 33 {
		return false
	}
	key, err := btcec.ParsePubKey(raw)

	return err == nil && bytes.Equal(raw, key.SerializeCompressed())
}

// validChallengeShape checks canonical v1 fields without imposing the client's
// wall clock on a challenge issued under the operator's clock.
func validChallengeShape(c *pb.NotificationChallenge) bool {
	return c != nil && len(c.ProtoReflect().GetUnknown()) == 0 &&
		canonicalPublicKey(c.OwnerPubkey) &&
		canonicalPublicKey(c.OperatorPubkey) &&
		len(c.Token) == 64 && c.ExpiresAtUnix > 0
}

// validChallenge applies the issuer's time policy at server verification.
func validChallenge(c *pb.NotificationChallenge, now time.Time) bool {
	return validChallengeShape(c) && c.ExpiresAtUnix > now.Unix() &&
		c.ExpiresAtUnix <= now.Add(ChallengeLifetime).Unix()
}

// validID bounds opaque identifiers and excludes delimiters, URLs and controls.
func validID(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '-' || c == '_' ||
			c == '.' {

			continue
		}

		return false
	}

	return true
}

// validCapability accepts the relay's exact 32-byte raw URL-base64 secrets.
func validCapability(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)

	return err == nil && len(decoded) == 32 &&
		base64.RawURLEncoding.EncodeToString(decoded) == value
}

// validateMutationShape checks the action and canonical fields without
// depending on a client's local clock. The server applies expiry policy.
func validateMutationShape(m *pb.NotificationSubscriptionMutation) error {
	if m == nil || len(m.ProtoReflect().GetUnknown()) != 0 ||
		!validID(m.SubscriptionId, 64) {
		return ErrInvalidAuthorization
	}
	switch m.Action {
	case pb.NotificationSubscriptionMutation_ACTION_UPSERT:
		if !validID(m.RelayId, 100) ||
			!validCapability(m.WakeCapability) ||
			m.AlertCapability != "" &&
				!validCapability(m.AlertCapability) ||
			m.ExpiresAtUnix <= 0 {
			return ErrInvalidAuthorization
		}

	case pb.NotificationSubscriptionMutation_ACTION_REMOVE:
		if m.RelayId != "" || m.WakeCapability != "" ||
			m.AlertCapability != "" || m.ExpiresAtUnix != 0 {
			return ErrInvalidAuthorization
		}

	default:
		return ErrInvalidAuthorization
	}

	return nil
}

// ValidateMutation applies server-side action and expiry policy. Its upper
// bound allows five minutes of operator/relay clock skew. The server must not
// persist an expiry beyond the grant verified at its configured relay.
func ValidateMutation(m *pb.NotificationSubscriptionMutation,
	now time.Time) error {

	if err := validateMutationShape(m); err != nil {
		return err
	}
	if m.Action == pb.NotificationSubscriptionMutation_ACTION_UPSERT &&
		(m.ExpiresAtUnix <= now.Unix() || m.ExpiresAtUnix >
			now.
				Add(
					MaxSubscriptionLifetime+
						subscriptionClockSkew,
				).
				Unix()) {
		return ErrInvalidAuthorization
	}

	return nil
}

// SigningMessage encodes v1 fields in fixed order. Byte/string fields have a
// uint32 big-endian length prefix; action is uint32 and expiries are int64
// big-endian. No JSON/protobuf re-encoding or map ordering affects the
// signature. Signature itself is excluded. Unknown fields are rejected for this
// version. This encoder is independent of wall clocks; server verification
// applies freshness and subscription expiry policy.
func SigningMessage(r *pb.NotificationSubscriptionRequest) ([]byte, error) {
	if r == nil || len(r.ProtoReflect().GetUnknown()) != 0 ||
		!validChallengeShape(r.Challenge) {
		return nil, ErrInvalidAuthorization
	}
	if err := validateMutationShape(r.Mutation); err != nil {
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
	for _, field := range []string{m.SubscriptionId, m.RelayId,
		m.WakeCapability, m.AlertCapability} {
		writeField([]byte(field))
	}
	_ = binary.Write(&buf, binary.BigEndian, m.ExpiresAtUnix)

	return buf.Bytes(), nil
}

// VerifyAuthorization verifies the mailbox owner's signature over every v1
// field. It does not replace VerifyChallenge or atomic replay consumption.
func VerifyAuthorization(r *pb.NotificationSubscriptionRequest,
	now time.Time) error {

	message, err := SigningMessage(r)
	if err != nil {
		return err
	}
	if !validChallenge(r.Challenge, now) {
		return ErrInvalidAuthorization
	}
	if err := ValidateMutation(r.Mutation, now); err != nil {
		return err
	}
	sig, err := schnorr.ParseSignature(r.Signature)
	if err != nil {
		return ErrInvalidAuthorization
	}
	key, err := btcec.ParsePubKey(r.Challenge.OwnerPubkey)
	if err != nil {
		return ErrInvalidAuthorization
	}
	digest := chainhash.TaggedHash([]byte(SignatureTag), message)
	if !sig.Verify(digest[:], key) {
		return ErrInvalidAuthorization
	}

	return nil
}
