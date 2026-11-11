package notifyrelay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// accountTestAuthenticator demonstrates a publisher module with authenticated
// account proofs rather than Apple evidence. Its test-only secret stays in
// publisher configuration; the store receives only an account selector.
type accountTestAuthenticator struct{ secret []byte }

// ValidateApplication accepts this fixture's configured audience without Apple
// application IDs or attestation environments.
func (a *accountTestAuthenticator) ValidateApplication(app AppConfig) error {
	if app.ID != "account-app" {
		return errors.New("unknown account audience")
	}

	return nil
}

// sign binds the account credential to the exact opaque transcript.
func (a *accountTestAuthenticator) sign(data []byte) []byte {
	digest := hmac.New(sha256.New, a.secret)
	_, _ = digest.Write(data)

	return digest.Sum(nil)
}

// Attest validates the account's enrollment transcript before returning public
// verification metadata. No Apple-specific certificate is interpreted here.
func (a *accountTestAuthenticator) Attest(ctx context.Context, _ AppConfig,
	proof []byte, keyID string, data []byte) (DeviceKey, error) {

	if err := ctx.Err(); err != nil {
		return DeviceKey{}, err
	}
	expected := sha256.Sum256([]byte("account:alice"))
	if keyID != base64.StdEncoding.EncodeToString(expected[:]) {
		return DeviceKey{}, errors.New("account credential identity " +
			"mismatch")
	}
	if !hmac.Equal(proof, a.sign(data)) {
		return DeviceKey{}, errors.New("invalid account proof")
	}

	return DeviceKey{PublicKey: []byte("account:alice")}, nil
}

// Assert verifies a counter bound to this challenge and exact payload. It
// deliberately leaves monotonic persistence enforcement to the store.
func (a *accountTestAuthenticator) Assert(ctx context.Context, _ AppConfig,
	key []byte, _ uint32, challenge Challenge, payload, proof []byte) (
	uint32, error) {

	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(proof) != 4+sha256.Size ||
		!bytes.Equal(key, []byte("account:alice")) {
		return 0, errors.New("invalid account assertion")
	}
	data := append(
		bytes.Clone(proof[:4]), ClientData(challenge, payload)...,
	)
	if !hmac.Equal(proof[4:], a.sign(data)) {
		return 0, errors.New("invalid account assertion")
	}

	return binary.BigEndian.Uint32(proof[:4]), nil
}

// accountProof creates a real fixture signature over a counter and transcript.
func (a *accountTestAuthenticator) accountProof(challenge Challenge,
	payload []byte, counter uint32) ProofRequest {

	prefix := binary.BigEndian.AppendUint32(nil, counter)
	signature := a.sign(
		append(
			bytes.Clone(prefix), ClientData(challenge, payload)...,
		),
	)

	return ProofRequest{ChallengeID: challenge.ID, Payload: payload,
		Proof: append(prefix, signature...)}
}

// opaqueTestProvider accepts case-sensitive opaque destinations unchanged.
type opaqueTestProvider struct{}

// ValidateApplication owns the fixture provider's destination policy.
func (p *opaqueTestProvider) ValidateApplication(app AppConfig) error {
	if app.Topic != "account-notices" {
		return errors.New("unknown destination policy")
	}

	return nil
}

// NormalizeDeviceToken validates a test prefix without applying hex rules.
func (p *opaqueTestProvider) NormalizeDeviceToken(token string) (string,
	error) {

	if !strings.HasPrefix(token, "FCM:") || len(token) < 5 ||
		len(token) > 4096 {
		return "", errors.New("invalid opaque destination")
	}

	return token, nil
}

// Send is not exercised by admission tests and performs no external request.
func (p *opaqueTestProvider) Send(context.Context, Delivery) (ProviderResult,
	error) {

	return ProviderResult{Outcome: OutcomeAccepted}, nil
}

// publisherPost submits the same public JSON API a publisher's app uses.
func publisherPost(t *testing.T, server *Server, path string,
	value any) *httptest.ResponseRecorder {

	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	request := httptest.NewRequest(
		http.MethodPost, path, bytes.NewReader(raw),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	return response
}

// TestPublisherAccountModuleAndOpaqueTokens proves the exported integration
// reaches real HTTP enrollment, assertions and atomic store updates without
// Apple configuration or accidental case normalization.
func TestPublisherAccountModuleAndOpaqueTokens(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "relay.db"))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, store.Close())
	}()
	auth := &accountTestAuthenticator{
		secret: []byte("account fixture secret"),
	}
	app := AppConfig{ID: "account-app", Topic: "account-notices"}
	config := Config{Applications: []AppConfig{app}, Authenticator: auth}
	server, err := NewServer(config, store, &opaqueTestProvider{})
	require.NoError(t, err)
	var challenge Challenge
	response := publisherPost(t, server, "/v1/challenges", ChallengeRequest{
		AppID: app.ID, Operation: OperationEnroll,
	})
	require.Equal(t, http.StatusOK, response.Code)
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &challenge))
	token := "FCM:Case/Sensitive+Value="
	payload, err := json.Marshal(EnrollmentPayload{DeviceToken: token})
	require.NoError(t, err)
	credential := sha256.Sum256([]byte("account:alice"))
	keyID := base64.StdEncoding.EncodeToString(credential[:])
	proof := ProofRequest{ChallengeID: challenge.ID, KeyID: keyID,
		Payload: payload,
		Proof:   auth.sign(ClientData(challenge, payload))}
	tampered := proof
	tampered.KeyID = base64.StdEncoding.EncodeToString(
		bytes.Repeat(
			[]byte{4}, 32,
		),
	)
	response = publisherPost(t, server, "/v1/installations", tampered)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	tampered = proof
	tampered.Payload = []byte(`{"device_token":"FCM:unapproved"}`)
	response = publisherPost(t, server, "/v1/installations", tampered)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	response = publisherPost(t, server, "/v1/installations", proof)
	require.Equal(t, http.StatusCreated, response.Code)
	var enrollment EnrollmentResult
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &enrollment))
	owner, err := store.loadInstallation(
		t.Context(), enrollment.InstallationID,
	)
	require.NoError(t, err)
	require.Equal(t, token, owner.DeviceToken)
	require.Equal(t, []byte("account:alice"), owner.PublicKey)
	// Enrollment replay cannot replace or reset this installation.
	response = publisherPost(t, server, "/v1/installations", proof)
	require.Equal(t, http.StatusConflict, response.Code)
	action, err := json.Marshal(
		ManagementPayload{
			Action:      "rotate_token",
			DeviceToken: "FCM:NEW/cAsE=",
		},
	)
	require.NoError(t, err)
	for _, counter := range []uint32{1, 1, 2} {
		response = publisherPost(
			t, server, "/v1/challenges", ChallengeRequest{
				AppID:          app.ID,
				Operation:      OperationManage,
				InstallationID: owner.ID,
			},
		)
		require.Equal(t, http.StatusOK, response.Code)
		require.NoError(
			t,
			json.Unmarshal(
				response.Body.Bytes(), &challenge,
			),
		)
		response = publisherPost(
			t, server, "/v1/installations/"+owner.ID+"/manage",
			auth.accountProof(challenge, action, counter),
		)
		if counter == 1 && owner.Counter == 1 {
			require.Equal(t, http.StatusConflict, response.Code)
			continue
		}
		require.Equal(t, http.StatusOK, response.Code)
		owner, err = store.loadInstallation(t.Context(), owner.ID)
		require.NoError(t, err)
		require.Equal(t, counter, owner.Counter)
		require.Equal(t, "FCM:NEW/cAsE=", owner.DeviceToken)
	}
	// Default admission still rejects this non-Apple policy. The reference
	// APNs provider independently rejects a missing APNs environment.
	_, err = NewServer(
		Config{
			Applications: []AppConfig{app},
		},
		store,
		&opaqueTestProvider{},
	)
	require.Error(t, err)
	_, err = NewServer(config, store, &APNSProvider{})
	require.Error(t, err)
}

// TestReferenceDevicePolicies preserves Apple's audience/environment and hex
// token boundaries after extracting publisher modules.
func TestReferenceDevicePolicies(t *testing.T) {
	auth, err := NewAppAttestAuthenticator()
	require.NoError(t, err)
	app := AppConfig{ID: "app", Topic: "com.example.wallet",
		AppleAppID:        "ABCDEFGHIJ.com.example.wallet",
		AttestEnvironment: "production",
		APNSEnvironment:   "development"}
	require.NoError(t, auth.ValidateApplication(app))
	provider := &APNSProvider{}
	require.NoError(t, provider.ValidateApplication(app))
	upper := strings.Repeat("AB", 32)
	normalized, err := provider.NormalizeDeviceToken(upper)
	require.NoError(t, err)
	require.Equal(t, strings.ToLower(upper), normalized)
	_, err = provider.NormalizeDeviceToken("FCM:OpaqueValue")
	require.Error(t, err)
	app.AppleAppID = "wrong.com.example.wallet"
	require.Error(t, auth.ValidateApplication(app))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = auth.Attest(ctx, app, nil, "", nil)
	require.ErrorIs(t, err, context.Canceled)
	_, err = auth.Assert(ctx, app, nil, 0, Challenge{}, nil, nil)
	require.ErrorIs(t, err, context.Canceled)
}
