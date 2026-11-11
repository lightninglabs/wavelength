package notifyrelay

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	attest "github.com/takimoto3/app-attest"
)

// AppConfig is immutable publisher routing and admission policy. ID selects
// the application alias. The reference App Attest/APNs modules also require
// AppleAppID, Topic and separate attestation/provider environments. Custom
// publisher modules validate their own audience and destination fields.
type AppConfig struct {
	ID                string `json:"id"`
	AppleAppID        string `json:"apple_app_id"`
	Topic             string `json:"topic"`
	AttestEnvironment string `json:"attest_environment"`
	APNSEnvironment   string `json:"apns_environment"`
}

// validateAttestation checks only the default Apple admission module's policy.
func (a AppConfig) validateAttestation() error {
	if !validIdentifier(a.ID, 100) || !validIdentifier(a.Topic, 255) ||
		!validIdentifier(a.AppleAppID, 266) ||
		len(strings.TrimSuffix(a.AppleAppID, "."+a.Topic)) != 10 ||
		!strings.HasSuffix(a.AppleAppID, "."+a.Topic) {
		return errors.New("app alias, Apple App ID and topic are " +
			"required")
	}
	if a.AttestEnvironment != "production" &&
		a.AttestEnvironment != "development" {
		return errors.New("invalid App Attest environment")
	}

	return nil
}

// verifiedKey contains public attestation evidence only. Counter persistence
// and challenge single use belong to the relay store, not the verifier.
type verifiedKey struct {
	publicKey []byte
	receipt   []byte
}

// installationVerifier separates cryptographic checks from atomic enrollment
// and counter updates. Production uses appAttestVerifier; tests inject a fake
// through an unexported constructor rather than offering an HTTP auth bypass.
type installationVerifier interface {
	// attest validates Apple trust, audience, key identity and environment.
	attest(AppConfig, []byte, string, []byte) (verifiedKey, error)

	// assert validates the exact signed request and a strictly newer
	// counter.
	assert(AppConfig, []byte, uint32, Challenge, []byte,
		[]byte) (uint32, error)
}

// appAttestVerifier uses a dedicated Apple App Attestation root pool. The
// public TLS/system root pool must never be substituted for that pool.
type appAttestVerifier struct {
	roots *x509.CertPool
}

// attest delegates Apple's certificate/nonce checks to the pinned verifier
// and enforces the configured development/production boundary independently.
func (v *appAttestVerifier) attest(app AppConfig, raw []byte, keyID string,
	clientData []byte) (verifiedKey, error) {

	var empty verifiedKey
	key, err := base64.StdEncoding.DecodeString(keyID)
	if err != nil || len(key) != sha256.Size {
		return empty, errors.New("invalid attestation key identifier")
	}
	var object attest.AttestationObject
	if err := object.UnmarshalCBOR(raw); err != nil {
		return empty, fmt.Errorf("decode attestation: %w", err)
	}
	hash := sha256.Sum256(clientData)
	service := attest.NewAttestationService(v.roots, app.AppleAppID)
	result, err := service.VerifyForiOS(
		&object, app.AppleAppID, hash[:], key,
	)
	if err != nil {
		return empty, fmt.Errorf("verify attestation: %w", err)
	}
	wanted := attest.Production
	if app.AttestEnvironment == "development" {
		wanted = attest.Development
	}
	if result.Environment != wanted {
		return empty, errors.New("attestation environment mismatch")
	}
	public, err := x509.MarshalPKIXPublicKey(result.PublicKey)
	if err != nil {
		return empty, err
	}

	return verifiedKey{publicKey: public, receipt: result.Receipt}, nil
}

// assert binds an App Attest assertion to saved public key/counter state and
// the relay-constructed transcript. Callers expose only a generic HTTP error;
// verifier diagnostics can include the expected challenge and remain private.
func (v *appAttestVerifier) assert(app AppConfig, encodedKey []byte,
	counter uint32, challenge Challenge, payload, raw []byte) (uint32,
	error) {

	parsed, err := x509.ParsePKIXPublicKey(encodedKey)
	if err != nil {
		return 0, err
	}
	key, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return 0, errors.New("installation key is not ECDSA")
	}
	var object attest.AssertionObject
	if err := object.UnmarshalCBOR(raw); err != nil {
		return 0, err
	}
	service := &attest.AssertionService{}
	result, err := service.VerifyWithState(
		&object, challenge.Nonce, ClientData(challenge, payload),
		attest.AssertionState{
			AppID: app.AppleAppID, PublicKey: key,
			Counter: counter, OriginalChallenge: challenge.Nonce,
		},
	)
	if err != nil {
		return 0, err
	}

	return result.Counter, nil
}

// validIdentifier keeps transcript fields unambiguous and limits routing state.
func validIdentifier(value string, maxLength int) bool {
	if value == "" || len(value) > maxLength {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '-' || character == '_' ||
			character == '.' {

			continue
		}

		return false
	}

	return true
}
