package notifyrelay

import (
	"context"
	"crypto/x509"
	"errors"
)

// DeviceKey contains only public enrollment evidence. The store owns challenge
// consumption, strict assertion counters and installation lifetime.
type DeviceKey struct {
	PublicKey []byte
	Receipt   []byte
}

// DeviceAuthenticator is publisher-controlled device admission. Implementations
// authenticate the exact relay transcript, application audience and credential
// identity. Credential IDs use the canonical 32-byte base64 InstallationID
// contract; that encoding does not itself prove any platform's trust.
// The reference daemon uses App Attest. Replacing this module is an explicit Go
// integration, never an HTTP option or command-line authentication bypass.
type DeviceAuthenticator interface {
	// ValidateApplication rejects invalid publisher audience/environment
	// policy.
	ValidateApplication(AppConfig) error

	// Attest verifies enrollment before any installation state is
	// allocated. Proof and clientData must not be logged. PublicKey is
	// verification material.
	Attest(context.Context, AppConfig, []byte, string,
		[]byte) (DeviceKey, error)

	// Assert verifies the exact request and returns its authenticated
	// counter. The store independently requires it to be strictly greater
	// than saved state.
	Assert(context.Context, AppConfig, []byte, uint32, Challenge, []byte,
		[]byte) (uint32, error)
}

// AppAttestAuthenticator implements publisher admission using Apple's dedicated
// App Attestation trust root, distinct from TLS/system certificate trust.
type AppAttestAuthenticator struct {
	verifier installationVerifier
}

// NewAppAttestAuthenticator restores the reference daemon's Apple trust policy.
// It accepts no caller-selected root or bypass flag.
func NewAppAttestAuthenticator() (*AppAttestAuthenticator, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(appAttestRoot) {
		return nil, errors.New("invalid embedded App Attest root")
	}

	return &AppAttestAuthenticator{
		verifier: &appAttestVerifier{
			roots: roots,
		},
	}, nil
}

// ValidateApplication checks Apple's audience, topic and attestation boundary.
// Delivery-provider validation separately owns destination environment policy.
func (a *AppAttestAuthenticator) ValidateApplication(app AppConfig) error {
	if a == nil || a.verifier == nil {
		return errors.New("uninitialized App Attest authenticator")
	}

	return app.validateAttestation()
}

// Attest retains the existing nonce, audience, key and environment
// verification.
func (a *AppAttestAuthenticator) Attest(ctx context.Context, app AppConfig,
	proof []byte, keyID string, clientData []byte) (DeviceKey, error) {

	if err := ctx.Err(); err != nil {
		return DeviceKey{}, err
	}
	key, err := a.verifier.attest(app, proof, keyID, clientData)
	if err != nil {
		return DeviceKey{}, err
	}

	return DeviceKey{PublicKey: key.publicKey, Receipt: key.receipt}, nil
}

// Assert retains the existing exact-payload signature and counter checks.
func (a *AppAttestAuthenticator) Assert(ctx context.Context, app AppConfig,
	key []byte, counter uint32, challenge Challenge, payload,
	proof []byte) (uint32, error) {

	if err := ctx.Err(); err != nil {
		return 0, err
	}

	return a.verifier.assert(app, key, counter, challenge, payload, proof)
}
