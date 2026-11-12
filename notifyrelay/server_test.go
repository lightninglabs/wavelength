package notifyrelay

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testEnrollmentVerifier replaces Apple certificate issuance only. Management
// assertions still pass through the production P-256/App-ID/counter verifier.
type testEnrollmentVerifier struct {
	appAttestVerifier
	public *ecdsa.PublicKey
}

// attest verifies a test key's signature over the same enrollment transcript.
func (v *testEnrollmentVerifier) attest(app AppConfig, proof []byte, _ string,
	clientData []byte) (verifiedKey, error) {

	hash := sha256.Sum256(
		append(
			[]byte(app.AppleAppID+"\x00"), clientData...,
		),
	)
	if !ecdsa.VerifyASN1(v.public, hash[:], proof) {
		return verifiedKey{}, errors.New("invalid test attestation")
	}
	key, err := x509.MarshalPKIXPublicKey(v.public)

	return verifiedKey{publicKey: key, receipt: []byte("test receipt")}, err
}

// testProvider drives delivery races without contacting an Apple endpoint.
type testProvider struct {
	send func(context.Context, Delivery) (ProviderResult, error)
}

// ValidateApplication preserves reference APNs routing policy in HTTP tests.
func (p *testProvider) ValidateApplication(app AppConfig) error {
	return (&APNSProvider{}).ValidateApplication(app)
}

// NormalizeDeviceToken preserves reference token validation in HTTP tests.
func (p *testProvider) NormalizeDeviceToken(token string) (string, error) {
	return (&APNSProvider{}).NormalizeDeviceToken(token)
}

// Send invokes the test's controlled provider outcome.
func (p *testProvider) Send(ctx context.Context, d Delivery) (ProviderResult,
	error) {

	return p.send(ctx, d)
}

// relayHarness holds a real database and real signed management assertions.
type relayHarness struct {
	t        *testing.T
	store    *SQLStore
	server   *Server
	provider *testProvider
	key      *ecdsa.PrivateKey
	keyID    string
	ownerID  string
	app      AppConfig
	clock    atomic.Int64
	counter  atomic.Uint32
}

// newRelayHarness isolates persistent state and fake certificate issuance.
func newRelayHarness(t *testing.T, shared *SQLStore) *relayHarness {
	t.Helper()
	h := &relayHarness{t: t, app: AppConfig{
		ID:                "wallet-production",
		AppleAppID:        "TESTTEAM01.com.example.wallet",
		Topic:             "com.example.wallet",
		AttestEnvironment: "production",
		APNSEnvironment:   "production",
	}}
	h.clock.Store(time.Now().UTC().UnixNano())
	if shared == nil {
		database := sqlTestOpener(t)()
		t.Cleanup(func() { require.NoError(t, database.Close()) })
		var err error
		shared, err = NewSQLStore(
			t.Context(), database, testSourceConfig(t, "producer"),
		)
		require.NoError(t, err)
	}
	h.store = shared
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	h.key = key
	digest := sha256.Sum256(elliptic.Marshal(key.Curve, key.X, key.Y))
	h.keyID = base64.StdEncoding.EncodeToString(digest[:])
	h.provider = &testProvider{send: func(context.Context, Delivery) (
		ProviderResult, error) {

		return ProviderResult{
			Outcome: OutcomeAccepted,
		}, nil
	}}
	h.server, err = newAuthenticatedServer(
		Config{Applications: []AppConfig{h.app}}, shared, h.provider,
		&AppAttestAuthenticator{verifier: &testEnrollmentVerifier{
			public: &key.PublicKey,
		}}, h.now,
	)
	require.NoError(t, err)
	h.enroll()

	return h
}

// now returns a race-safe clock advanced explicitly by each scenario.
func (h *relayHarness) now() time.Time {
	return time.Unix(0, h.clock.Load())
}

// post executes the actual HTTP handler without opening a network socket.
func (h *relayHarness) post(path string, value any,
	capability string) *httptest.ResponseRecorder {

	h.t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(h.t, err)
	request := httptest.NewRequest(
		http.MethodPost, path, bytes.NewReader(raw),
	)
	if capability != "" {
		request.Header.Set("Authorization", "Bearer "+capability)
	}
	response := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(response, request)

	return response
}

// challenge obtains a nonce scoped to this app and one management operation.
func (h *relayHarness) challenge(operation string) Challenge {
	h.t.Helper()
	request := ChallengeRequest{AppID: h.app.ID, Operation: operation}
	if operation == OperationManage {
		request.InstallationID = h.ownerID
	}
	response := h.post("/v1/challenges", request, "")
	require.Equal(h.t, http.StatusOK, response.Code)
	var result Challenge
	require.NoError(h.t, json.Unmarshal(response.Body.Bytes(), &result))

	return result
}

// enroll verifies the complete HTTP enrollment/persistence path with a test CA.
func (h *relayHarness) enroll() {
	h.t.Helper()
	challenge := h.challenge(OperationEnroll)
	payload, err := json.Marshal(EnrollmentPayload{DeviceToken: "aabbccdd"})
	require.NoError(h.t, err)
	hash := sha256.Sum256(
		append(
			[]byte(h.app.AppleAppID+"\x00"),
			ClientData(challenge, payload)...,
		),
	)
	proof, err := ecdsa.SignASN1(rand.Reader, h.key, hash[:])
	require.NoError(h.t, err)
	response := h.post("/v1/installations", ProofRequest{
		ChallengeID: challenge.ID, KeyID: h.keyID,
		Proof: proof, Payload: payload,
	}, "")
	require.Equal(h.t, http.StatusCreated, response.Code)
	var result EnrollmentResult
	require.NoError(h.t, json.Unmarshal(response.Body.Bytes(), &result))
	h.ownerID = result.InstallationID
	expected, err := InstallationID(h.app.ID, h.keyID)
	require.NoError(h.t, err)
	require.Equal(h.t, expected, h.ownerID)
}

// assertion generates Apple's CBOR assertion shape with a real P-256 signature.
// Two SHA256 steps follow the pinned verifier's Apple guide/golden vectors.
func (h *relayHarness) assertion(challenge Challenge, payload []byte,
	counter uint32, audience string) ProofRequest {

	h.t.Helper()
	auth := make([]byte, 37)
	rpid := sha256.Sum256([]byte(audience))
	copy(auth, rpid[:])
	binary.BigEndian.PutUint32(auth[33:], counter)
	dataHash := sha256.Sum256(ClientData(challenge, payload))
	nonce := sha256.Sum256(append(bytes.Clone(auth), dataHash[:]...))
	digest := sha256.Sum256(nonce[:])
	sig, err := ecdsa.SignASN1(rand.Reader, h.key, digest[:])
	require.NoError(h.t, err)
	// The two byte strings are shorter than 256 bytes. This is test-only
	// CBOR encoding, independent of the production decoder.
	raw := []byte{0xa2, 0x69}
	raw = append(raw, []byte("signature")...)
	raw = append(raw, 0x58, byte(len(sig)))
	raw = append(raw, sig...)
	raw = append(raw, 0x71)
	raw = append(raw, []byte("authenticatorData")...)
	raw = append(raw, 0x58, byte(len(auth)))
	raw = append(raw, auth...)

	return ProofRequest{
		ChallengeID: challenge.ID,
		Payload:     payload,
		Proof:       raw,
	}
}

// manage signs and submits a fresh operation using a monotonic device counter.
func (h *relayHarness) manage(
	action ManagementPayload) *httptest.ResponseRecorder {

	h.t.Helper()
	challenge := h.challenge(OperationManage)
	payload, err := json.Marshal(action)
	require.NoError(h.t, err)
	proof := h.assertion(
		challenge, payload, h.counter.Add(1), h.app.AppleAppID,
	)

	return h.post("/v1/installations/"+h.ownerID+"/manage", proof, "")
}

// subscribe creates one app-owned route and decodes its source offers.
func (h *relayHarness) subscribe(route string, alerts bool) ManagementResult {
	h.t.Helper()
	response := h.manage(
		ManagementPayload{
			Action:       "subscribe",
			RouteID:      route,
			EnableAlerts: alerts,
		},
	)
	require.Equal(h.t, http.StatusOK, response.Code)
	var result ManagementResult
	require.NoError(h.t, json.Unmarshal(response.Body.Bytes(), &result))

	return result
}

// TestManagementProofBinding proves mutation/audience changes and nonce replay
// fail even when the attacker supplies a fresh, otherwise valid counter.
func TestManagementProofBinding(t *testing.T) {
	h := newRelayHarness(t, nil)
	challenge := h.challenge(OperationManage)
	payload, err := json.Marshal(
		ManagementPayload{
			Action:  "subscribe",
			RouteID: "wallet-a",
		},
	)
	require.NoError(t, err)
	proof := h.assertion(challenge, payload, 1, h.app.AppleAppID)
	mutated := proof
	mutated.Payload = []byte(`{"action":"subscribe","route_id":"wallet-b"}`)
	path := "/v1/installations/" + h.ownerID + "/manage"
	require.Equal(
		t, http.StatusUnauthorized, h.post(path, mutated, "").Code,
	)
	wrongApp := h.assertion(
		challenge, payload, 1, "OTHERTEAM1.com.example.wallet",
	)
	require.Equal(
		t, http.StatusUnauthorized, h.post(path, wrongApp, "").Code,
	)
	require.Equal(t, http.StatusOK, h.post(path, proof, "").Code)
	require.Equal(t, http.StatusUnauthorized, h.post(path, proof, "").Code)
	// The nonce itself is one-use, beyond the assertion counter fence.
	freshCounter := h.assertion(challenge, payload, 2, h.app.AppleAppID)
	require.Equal(
		t, http.StatusConflict, h.post(path, freshCounter, "").Code,
	)
	owner, err := h.store.loadInstallation(t.Context(), h.ownerID)
	require.NoError(t, err)
	require.EqualValues(t, 1, owner.Counter)
}

// TestConcurrentManagementCounter proves two valid proofs cannot both commit
// the same counter, and the losing action creates no subscription.
func TestConcurrentManagementCounter(t *testing.T) {
	h := newRelayHarness(t, nil)
	proofs := make([]ProofRequest, 2)
	for i, route := range []string{"wallet-a", "wallet-b"} {
		payload, err := json.Marshal(
			ManagementPayload{
				Action:  "subscribe",
				RouteID: route,
			},
		)
		require.NoError(t, err)
		proofs[i] = h.assertion(
			h.challenge(OperationManage), payload, 1,
			h.app.AppleAppID,
		)
	}
	var successes atomic.Int32
	var workers sync.WaitGroup
	for _, proof := range proofs {
		workers.Go(func() {
			response := h.post(
				"/v1/installations/"+h.ownerID+"/manage", proof,
				"",
			)
			if response.Code == http.StatusOK {
				successes.Add(1)
			}
		})
	}
	workers.Wait()
	require.EqualValues(t, 1, successes.Load())
	count, err := h.store.database.Queries.CountInstallationSubscriptions(
		t.Context(), h.ownerID,
	)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
}

// TestAnonymousChallengesDoNotAllocate verifies stateless challenges survive
// a real SQL pool restart without allocating consumed-nonce rows.
func TestAnonymousChallengesDoNotAllocate(t *testing.T) {
	open := sqlTestOpener(t)
	database := open()
	store, err := NewSQLStore(t.Context(), database)
	require.NoError(t, err)
	now := time.Now()
	var challenge Challenge
	for range 1024 {
		challenge, err = store.NewChallenge(
			t.Context(), ChallengeRequest{
				AppID:     "wallet",
				Operation: OperationEnroll,
			},
			now,
		)
		require.NoError(t, err)
	}
	count, err := database.Queries.CountConsumedChallenges(t.Context())
	require.NoError(t, err)
	require.Zero(t, count)
	require.NoError(t, database.Close())
	database = open()
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	store, err = NewSQLStore(t.Context(), database)
	require.NoError(t, err)
	loaded, err := store.loadChallenge(t.Context(), challenge.ID, now)
	require.NoError(t, err)
	require.Equal(t, challenge, loaded)
	_, err = store.loadChallenge(t.Context(), challenge.ID+"x", now)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = store.loadChallenge(
		t.Context(), challenge.ID, now.Add(challengeLifetime),
	)
	require.ErrorIs(t, err, ErrNotFound)
}
