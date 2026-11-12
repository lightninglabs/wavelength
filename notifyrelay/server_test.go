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
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
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

// ValidateApplication preserves reference APNs routing policy in legacy tests.
func (p *testProvider) ValidateApplication(app AppConfig) error {
	return (&APNSProvider{}).ValidateApplication(app)
}

// NormalizeDeviceToken preserves reference token validation in legacy tests.
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
	store    *Store
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
func newRelayHarness(t *testing.T, shared *Store) *relayHarness {
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
		var err error
		shared, err = OpenStore(filepath.Join(t.TempDir(), "relay.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, shared.Close()) })
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
	h.server, err = newServer(
		Config{
			Applications: []AppConfig{h.app},
		},
		shared,
		h.provider, &testEnrollmentVerifier{
			public: &key.PublicKey,
		},
		h.now,
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

// subscribe creates one app-owned route and decodes its scoped capabilities.
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
	require.NoError(
		t, h.store.db.View(func(tx *bolt.Tx) error {
			require.Equal(
				t, 1, tx.Bucket(
					[]byte(subscriptionsBucket),
				).Stats().KeyN,
			)

			return nil
		}),
	)
}

// TestCapabilitiesAreScopedAndRevocable checks fixed routing, ownership and
// permission removal through the real HTTP and durable admission paths.
func TestCapabilitiesAreScopedAndRevocable(t *testing.T) {
	h := newRelayHarness(t, nil)
	other := newRelayHarness(t, h.store)
	sub := h.subscribe("wallet-a", true)
	foreign := other.subscribe("wallet-b", false)
	request := PushRequest{EventID: "event-1"}
	require.Equal(
		t, http.StatusUnauthorized,
		h.post("/v1/alerts", request, sub.WakeCapability).Code,
	)
	require.Equal(
		t, http.StatusAccepted,
		h.post("/v1/alerts", request, sub.AlertCapability).Code,
	)
	require.Equal(
		t, http.StatusBadRequest,
		h.post("/v1/wakes", map[string]string{
			"event_id": "event-2", "device_token": "deadbeef",
		}, sub.WakeCapability).Code,
	)
	require.Equal(
		t, http.StatusUnauthorized, h.manage(ManagementPayload{
			Action:         "revoke_subscription",
			SubscriptionID: foreign.SubscriptionID,
		}).Code,
	)
	// Disabling alerts also prevents a queued alert from being sent later.
	renewed := h.subscribe("wallet-a", false)
	require.Empty(t, renewed.AlertCapability)
	require.Equal(t, sub.SubscriptionID, renewed.SubscriptionID)
	require.Equal(
		t, http.StatusUnauthorized,
		h.post("/v1/wakes", request, sub.WakeCapability).Code,
	)
	worked, err := h.server.DeliverOnce(t.Context())
	require.NoError(t, err)
	require.False(t, worked)
	require.Equal(
		t, http.StatusAccepted,
		h.post("/v1/wakes", request, renewed.WakeCapability).Code,
	)
	require.Equal(
		t, http.StatusOK, h.manage(ManagementPayload{
			Action:         "revoke_subscription",
			SubscriptionID: renewed.SubscriptionID,
		}).Code,
	)
	worked, err = h.server.DeliverOnce(t.Context())
	require.NoError(t, err)
	require.False(t, worked)
}

// TestAnonymousChallengesDoNotAllocate verifies MAC/expiry validation and
// persistence across restart without an attacker-controlled nonce table.
func TestAnonymousChallengesDoNotAllocate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	store, err := OpenStore(path)
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
	require.NoError(
		t, store.db.View(func(tx *bolt.Tx) error {
			require.Zero(
				t, tx.Bucket(
					[]byte(challengesBucket),
				).Stats().KeyN,
			)

			return nil
		}),
	)
	require.NoError(t, store.Close())
	store, err = OpenStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
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

// TestSubscriptionVerification proves a producer can reject fabricated grants
// and mixed subscriptions without sending a notification or creating any work.
func TestSubscriptionVerification(t *testing.T) {
	h := newRelayHarness(t, nil)
	first := h.subscribe("first-route", true)
	second := h.subscribe("second-route", true)
	path := "/v1/subscriptions/verify"
	request := SubscriptionVerificationRequest{
		SubscriptionID:  first.SubscriptionID,
		AlertCapability: first.AlertCapability,
	}
	response := h.post(path, request, first.WakeCapability)
	require.Equal(t, http.StatusOK, response.Code)
	var result SubscriptionVerificationResult
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.Equal(t, first.ExpiresAt, result.ExpiresAt)
	worked, err := h.server.DeliverOnce(t.Context())
	require.NoError(t, err)
	require.False(t, worked, "verification must never create a push job")
	require.NotContains(t, response.Body.String(), h.ownerID)
	request.AlertCapability = second.AlertCapability
	require.Equal(
		t, http.StatusUnauthorized,
		h.post(path, request, first.WakeCapability).Code,
	)
	request.AlertCapability = first.AlertCapability
	request.SubscriptionID = second.SubscriptionID
	require.Equal(
		t, http.StatusUnauthorized,
		h.post(path, request, first.WakeCapability).Code,
	)
	request.SubscriptionID = first.SubscriptionID
	require.Equal(
		t, http.StatusUnauthorized,
		h.post(path, request, first.AlertCapability).Code,
	)
	request.AlertCapability = ""
	require.Equal(
		t, http.StatusOK,
		h.post(path, request, first.WakeCapability).Code,
	)
	renewed := h.subscribe("first-route", false)
	require.Equal(
		t, http.StatusUnauthorized,
		h.post(path, request, first.WakeCapability).Code,
	)
	require.Equal(
		t, http.StatusOK,
		h.post(path, request, renewed.WakeCapability).Code,
	)
	request.AlertCapability = first.AlertCapability
	require.Equal(
		t, http.StatusUnauthorized,
		h.post(path, request, renewed.WakeCapability).Code,
	)
	request.AlertCapability = ""
	h.clock.Add(int64(subscriptionLifetime))
	require.Equal(
		t, http.StatusUnauthorized,
		h.post(path, request, renewed.WakeCapability).Code,
	)
}
