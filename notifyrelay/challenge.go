package notifyrelay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// challengeAuthority issues stateless, store-owned management challenges.
// Both storage implementations consume successful uses atomically themselves.
type challengeAuthority struct{ challengeKey [32]byte }

// NewChallenge issues a stateless MAC-bound nonce. Anonymous requests allocate
// no durable row; only a successfully authenticated use enters the replay set.
func (s *challengeAuthority) NewChallenge(ctx context.Context,
	req ChallengeRequest, now time.Time) (Challenge, error) {

	if err := ctx.Err(); err != nil {
		return Challenge{}, err
	}
	nonce, err := randomSecret(32)
	if err != nil {
		return Challenge{}, err
	}
	value := Challenge{Nonce: nonce, AppID: req.AppID,
		Operation: req.Operation, InstallationID: req.InstallationID,
		ExpiresAt: now.Add(challengeLifetime).Unix()}
	raw, err := json.Marshal(value)
	if err != nil {
		return Challenge{}, err
	}
	mac := hmac.New(sha256.New, s.challengeKey[:])
	_, _ = mac.Write(raw)
	value.ID = base64.RawURLEncoding.EncodeToString(raw) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return value, nil
}

// loadChallenge validates a self-contained server nonce without allocating
// state. Its successful use is still checked atomically with the mutation.
func (s *challengeAuthority) loadChallenge(ctx context.Context, id string,
	now time.Time) (Challenge, error) {

	if err := ctx.Err(); err != nil {
		return Challenge{}, err
	}
	if len(id) > 1024 {
		return Challenge{}, ErrNotFound
	}
	payload, signature, ok := strings.Cut(id, ".")
	if !ok {
		return Challenge{}, ErrNotFound
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Challenge{}, ErrNotFound
	}
	actual, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return Challenge{}, ErrNotFound
	}
	mac := hmac.New(sha256.New, s.challengeKey[:])
	_, _ = mac.Write(raw)
	if !hmac.Equal(actual, mac.Sum(nil)) {
		return Challenge{}, ErrNotFound
	}
	var value Challenge
	if err := json.Unmarshal(raw, &value); err != nil {
		return Challenge{}, err
	}
	if value.ID != "" || value.ExpiresAt <= now.Unix() {
		return Challenge{}, ErrNotFound
	}
	value.ID = id

	return value, nil
}
