package notifyrelay

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"time"
)

// AndroidAuthenticator admits tester APKs with a Google-verified signer.
// Separately configured emulator aliases require a publisher-paired key.
type AndroidAuthenticator struct {
	client *http.Client
	keys   map[string]map[string]bool
}

// androidEnrollmentProof binds a Keystore public key to the enrollment.
type androidEnrollmentProof struct {
	// Integrity carries the Play Integrity token for signer-based
	// admission.
	Integrity string `json:"integrity,omitempty"`
	// Key is the DER-encoded P-256 SubjectPublicKeyInfo.
	Key []byte `json:"key"`
	// Signature is the DER-encoded ECDSA signature over the client data.
	Signature []byte `json:"signature"`
}

// NewAndroidAuthenticator copies the app-specific approved key fingerprints.
// Each fingerprint is standard padded base64 of SHA256(DER
// SubjectPublicKeyInfo).
func NewAndroidAuthenticator(pins map[string][]string,
	transport http.RoundTripper) (*AndroidAuthenticator, error) {

	a := &AndroidAuthenticator{keys: make(map[string]map[string]bool)}
	if transport != nil {
		a.client = &http.Client{
			Timeout:   8 * time.Second,
			Transport: transport,
			CheckRedirect: func(*http.Request,
				[]*http.Request) error {

				return http.ErrUseLastResponse
			},
		}
	}
	for alias, ids := range pins {
		if !validIdentifier(alias, 100) || len(ids) == 0 {
			return nil, errors.New("invalid Android pairing policy")
		}
		keys := make(map[string]bool)
		for _, id := range ids {
			b, err := base64.StdEncoding.Strict().DecodeString(id)
			if err != nil || len(b) != sha256.Size ||
				base64.StdEncoding.EncodeToString(b) != id {
				return nil, errors.New("invalid paired " +
					"Android key ID")
			}
			keys[id] = true
		}
		a.keys[alias] = keys
	}

	return a, nil
}

// ValidateApplication requires one admission policy per Android alias.
func (a *AndroidAuthenticator) ValidateApplication(app AppConfig) error {
	if app.AndroidSigner != "" {
		b, err := base64.RawURLEncoding.Strict().DecodeString(
			app.AndroidSigner,
		)
		if err != nil || len(b) != sha256.Size || a.client == nil ||
			len(a.keys[app.ID]) != 0 {
			return errors.New("invalid Android signer policy")
		}
	} else if len(a.keys[app.ID]) == 0 {
		return errors.New("missing Android admission policy")
	}
	if app.AndroidPackage == "" ||
		app.AppleAppID != "" || app.Topic != "" ||
		app.AttestEnvironment != "" || app.APNSEnvironment != "" {
		return errors.New("invalid Android admission policy")
	}

	return nil
}

// androidKeyID derives the public fingerprint shown during local pairing.
func androidKeyID(key []byte) string {
	hash := sha256.Sum256(key)

	return base64.StdEncoding.EncodeToString(hash[:])
}

// verifyAndroidSignature accepts DER-encoded P-256 ECDSA signatures from the
// Android Keystore. A signature covers the exact bytes supplied by the relay.
func verifyAndroidSignature(key, transcript, signature []byte) bool {
	parsed, err := x509.ParsePKIXPublicKey(key)
	if err != nil {
		return false
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return false
	}
	digest := sha256.Sum256(transcript)

	return ecdsa.VerifyASN1(pub, digest[:], signature)
}

// Attest verifies possession of the installation key and checks either the APK
// signer through Play Integrity or the emulator key against the pairing list.
func (a *AndroidAuthenticator) Attest(ctx context.Context, app AppConfig,
	proof []byte, keyID string, clientData []byte) (DeviceKey, error) {

	var evidence androidEnrollmentProof
	if decodeJSON(bytes.NewReader(proof), &evidence) != nil ||
		androidKeyID(evidence.Key) != keyID ||
		!verifyAndroidSignature(
			evidence.Key, clientData, evidence.Signature,
		) {
		return DeviceKey{}, errors.New("invalid Android enrollment")
	}
	if app.AndroidSigner != "" {
		if err := a.checkIntegrity(
			ctx, app, keyID, clientData, evidence.Integrity,
		); err != nil {
			return DeviceKey{}, err
		}
	} else if !a.keys[app.ID][keyID] {
		return DeviceKey{}, errors.New("unknown paired APK " +
			"installation")
	}

	return DeviceKey{PublicKey: evidence.Key}, nil
}

// Assert verifies a signed four-byte big-endian counter followed by the
// canonical challenge transcript. SQL fences concurrent and replayed updates.
func (a *AndroidAuthenticator) Assert(_ context.Context, app AppConfig,
	key []byte, previous uint32, challenge Challenge, payload,
	proof []byte) (uint32, error) {

	if len(proof) <= 4 ||
		(app.AndroidSigner == "" &&
			!a.keys[app.ID][androidKeyID(key)]) {
		return 0, errors.New("unknown Android credential")
	}
	counter := binary.BigEndian.Uint32(proof[:4])
	data := append(
		bytes.Clone(proof[:4]), ClientData(challenge, payload)...,
	)
	if counter <= previous ||
		!verifyAndroidSignature(key, data, proof[4:]) {
		return 0, errors.New("invalid Android assertion")
	}

	return counter, nil
}
