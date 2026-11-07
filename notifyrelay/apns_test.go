package notifyrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// providerTransport captures provider requests without making network calls.
type providerTransport struct {
	request  *http.Request
	body     []byte
	calls    int
	status   int
	response string
}

// RoundTrip captures the actual request and returns a controlled APNs result.
func (p *providerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p.calls++
	p.request = r
	var err error
	p.body, err = io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if p.status == 0 {
		return nil, errors.New("private device URL: " + r.URL.String())
	}

	return &http.Response{
		StatusCode: p.status,
		Body:       io.NopCloser(strings.NewReader(p.response)),
		Header: http.Header{
			"Location": {
				"https://untrusted.example/",
			},
			"Retry-After": {
				"30",
			},
		},
	}, nil
}

// newTestAPNSProvider builds a real JWT signer around a capturing transport.
func newTestAPNSProvider(t *testing.T,
	transport *providerTransport) (*APNSProvider, *ecdsa.PublicKey) {

	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	provider, err := NewAPNSProvider(APNSConfig{
		TeamID: "TESTTEAM01", KeyID: "TESTKEY001", PrivateKey: key,
		HTTPClient: &http.Client{Transport: transport},
	})
	require.NoError(t, err)

	return provider, &key.PublicKey
}

// verifyProviderJWT checks Apple's raw ES256 signature and publisher issuer.
func verifyProviderJWT(t *testing.T, key *ecdsa.PublicKey, header string) {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(header, "bearer "), ".")
	require.Len(t, parts, 3)
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	require.Len(t, signature, 64)
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	require.True(t, ecdsa.Verify(key, digest[:], r, s))
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claim map[string]any
	require.NoError(t, json.Unmarshal(claims, &claim))
	require.Equal(t, "TESTTEAM01", claim["iss"])
}

// TestAPNSRequest verifies JWTs and fixed destinations/payloads for both
// scopes. The test never contacts Apple's service.
func TestAPNSRequest(t *testing.T) {
	for _, scope := range []Scope{ScopeWake, ScopeAlert} {
		t.Run(string(scope), func(t *testing.T) {
			verifyAPNSRequest(t, scope)
		})
	}
}

// verifyAPNSRequest checks one real serialized request with fake transport.
func verifyAPNSRequest(t *testing.T, scope Scope) {
	t.Helper()
	delivery := Delivery{
		DeviceToken: "aabbccdd", Topic: "com.example.wallet",
		Environment: "production", Scope: scope,
		RouteID: "opaque-route", EventHash: "event-hash",
		CollapseID: "collapse", ExpiresAt: time.Now().Add(time.Hour),
	}
	transport := &providerTransport{status: http.StatusOK}
	provider, key := newTestAPNSProvider(t, transport)
	result, err := provider.Send(t.Context(), delivery)
	require.NoError(t, err)
	require.Equal(t, OutcomeAccepted, result.Outcome)
	require.Equal(t, 1, transport.calls)
	r := transport.request
	require.Equal(
		t, "https://api.push.apple.com/3/device/aabbccdd",
		r.URL.String(),
	)
	require.Equal(t, delivery.Topic, r.Header.Get("Apns-Topic"))
	require.Equal(t, "5", r.Header.Get("Apns-Priority"))
	require.Equal(t, providerRequestID(delivery), r.Header.Get("Apns-Id"))
	verifyProviderJWT(t, key, r.Header.Get("Authorization"))
	var payload map[string]map[string]any
	require.NoError(t, json.Unmarshal(transport.body, &payload))
	require.Equal(t, float64(1), payload["aps"]["content-available"])
	require.Equal(t, delivery.RouteID, payload["wavelength"]["route_id"])
	require.NotContains(t, payload["wavelength"], "amount")
	if scope == ScopeWake {
		require.Equal(t, "background", r.Header.Get("Apns-Push-Type"))
		require.NotContains(t, payload["aps"], "alert")
	} else {
		require.Equal(t, "alert", r.Header.Get("Apns-Push-Type"))
		require.Contains(t, payload["aps"], "alert")
	}
}

// TestAPNSFailures proves redirect denial, retry classification and token-safe
// transport errors. A failure must not leak a device URL into diagnostics.
func TestAPNSFailures(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		outcome Outcome
	}{
		{
			"redirect",
			307,
			"",
			OutcomeRejected,
		},
		{"invalid-token", 410,
			`{"reason":"Unregistered","timestamp":123000}`,
			OutcomeInvalidToken},
		{
			"throttled",
			429,
			`{"reason":"TooManyRequests"}`,
			OutcomeRetry,
		},
		{"authorization", 403, `{"reason":"ExpiredProviderToken"}`,
			OutcomeRetry},
		{
			"transport",
			0,
			"",
			OutcomeRetry,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			transport := &providerTransport{
				status: test.status, response: test.body,
			}
			provider, _ := newTestAPNSProvider(t, transport)
			result, err := provider.Send(
				context.Background(), Delivery{
					DeviceToken: "deadbeef",
					Topic:       "com.example.wallet",
					Environment: "development",
					Scope:       ScopeWake,
				},
			)
			require.Equal(t, test.outcome, result.Outcome)
			require.Equal(t, 1, transport.calls)
			require.Equal(
				t, "api.sandbox.push.apple.com",
				transport.request.URL.Host,
			)
			if err != nil {
				require.NotContains(t, err.Error(), "deadbeef")
				require.NotContains(t, err.Error(), "https://")
			}
			if test.status == 410 {
				require.Equal(
					t, time.UnixMilli(123000),
					result.InvalidAt,
				)
			}
			if test.status == 429 {
				require.Equal(
					t, 30*time.Second, result.RetryAfter,
				)
			}
		})
	}
}
