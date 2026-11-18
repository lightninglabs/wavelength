package notifyrelay

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestAndroidPairing checks APK pairing, exact transcript ownership and
// authenticated counter ordering independently of the store's replay fence.
func TestAndroidPairing(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	raw, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	id := androidKeyID(raw)
	app := AppConfig{ID: "android", AndroidPackage: "example.wallet"}
	auth, err := NewAndroidAuthenticator(
		map[string][]string{
			app.ID: {id},
		},
		nil,
	)
	require.NoError(t, err)
	require.NoError(t, auth.ValidateApplication(app))
	sign := func(data []byte) []byte {
		digest := sha256.Sum256(data)
		sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
		require.NoError(t, err)

		return sig
	}
	challenge := Challenge{
		Operation: OperationEnroll,
		AppID:     app.ID,
		Nonce:     "one",
	}
	payload := []byte(`{"device_token":"private"}`)
	data := ClientData(challenge, payload)
	proof, err := json.Marshal(
		androidEnrollmentProof{
			Key:       raw,
			Signature: sign(data),
		},
	)
	require.NoError(t, err)
	result, err := auth.Attest(t.Context(), app, proof, id, data)
	require.NoError(t, err)
	require.Equal(t, raw, result.PublicKey)
	_, err = auth.Attest(
		t.Context(), app, proof, id, ClientData(challenge, nil),
	)
	require.Error(t, err)
	_, err = auth.Attest(
		t.Context(), AppConfig{
			ID: "foreign",
		},
		proof,
		id,
		data,
	)
	require.Error(t, err)

	challenge.Operation = OperationManage
	counter := binary.BigEndian.AppendUint32(nil, 5)
	proof = append(
		bytes.Clone(counter),
		sign(append(counter, ClientData(challenge, payload)...))...,
	)
	n, err := auth.Assert(
		t.Context(), app, raw, 4, challenge, payload, proof,
	)
	require.NoError(t, err)
	require.EqualValues(t, 5, n)
	_, err = auth.Assert(
		t.Context(), app, raw, 5, challenge, payload, proof,
	)
	require.Error(t, err)
	proof[3]++
	_, err = auth.Assert(
		t.Context(), app, raw, 4, challenge, payload, proof,
	)
	require.Error(t, err)
}

// TestFCMDelivery checks that silent wakes preserve the reason, target and
// deadline and that only explicit destination failures retire an installation.
func TestFCMDelivery(t *testing.T) {
	wire := &providerTransport{status: http.StatusOK}
	p := NewFCMProvider(wire)
	d := Delivery{policy: AppConfig{
		FirebaseProject: "example-project",
		AndroidPackage:  "example.wallet",
	},
		DeviceToken: "cAbcdefghijklmnopqrstu",
		Scope:       ScopeWake,
		RouteID:     "local",
		ExpiresAt:   time.Now().Add(30 * 24 * time.Hour), job: relayJob{
			Scope:            ScopeSchedule,
			WakeDeadlineUnix: 12345,
		},
	}
	result, err := p.Send(t.Context(), d)
	require.NoError(t, err)
	require.Equal(t, OutcomeAccepted, result.Outcome)
	// Compare against the provider's wire schema independently of the Go
	// types.
	require.JSONEq(t, `{
		"message": {
			"fid": "cAbcdefghijklmnopqrstu",
			"data": {
				"version": "1",
				"route_id": "local",
				"kind": "wake",
				"wake_reason": "scheduled_round",
				"wake_deadline_unix": "12345"
			},
			"android": {
				"priority": "normal", "ttl": "2419200s",
				"restricted_package_name": "example.wallet"
			}
		}
	}`, string(wire.body))
	require.Equal(
		t, "https://fcm.googleapis.com/v1/projects/example-project/m"+
			"essages:send", wire.request.URL.String(),
	)

	wire.status = http.StatusNotFound
	result, _ = p.Send(t.Context(), d)
	require.Equal(t, OutcomeRetry, result.Outcome)
	wire.response = `{"error":{"details":[{"@type":"` + fcmErrorType +
		`","errorCode":"UNREGISTERED"}]}}`
	result, err = p.Send(t.Context(), d)
	require.NoError(t, err)
	require.Equal(t, OutcomeInvalidToken, result.Outcome)
}

// TestAndroidTesterIntegrity admits independently installed APKs signed by
// the publisher and rejects a foreign certificate, key, nonce or device.
func TestAndroidTesterIntegrity(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	raw, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	id := androidKeyID(raw)
	data := []byte("relay transcript")
	hash := sha256.Sum256(data)
	sig, err := ecdsa.SignASN1(rand.Reader, key, hash[:])
	require.NoError(t, err)
	proof, err := json.Marshal(
		androidEnrollmentProof{
			Key:       raw,
			Signature: sig,
			Integrity: "google-verdict",
		},
	)
	require.NoError(t, err)
	signer := sha256.Sum256([]byte("tester APK signer"))
	app := AppConfig{ID: "android", AndroidPackage: "example.wallet",
		AndroidSigner: base64.RawURLEncoding.EncodeToString(signer[:])}
	wire := &providerTransport{status: http.StatusOK}
	auth, err := NewAndroidAuthenticator(nil, wire)
	require.NoError(t, err)
	require.NoError(t, auth.ValidateApplication(app))
	nonce := sha256.Sum256(append([]byte(id+"\n"), data...))
	// Decode a provider-shaped fixture once so a renamed JSON tag fails the
	// positive test even if the Go encoder and decoder change together.
	now := time.Now()
	wire.response = fmt.Sprintf(`{
		"tokenPayloadExternal": {
			"requestDetails": {
				"requestPackageName":"example.wallet",
				"nonce":%q,"timestampMillis":"%d"},
			"appIntegrity": {"packageName":"example.wallet",
				"appRecognitionVerdict":"UNRECOGNIZED_VERSION",
				"certificateSha256Digest":[%q]},
			"deviceIntegrity": {
				"deviceRecognitionVerdict":[
					"MEETS_DEVICE_INTEGRITY"
				]
			}
		}
	}`,
		base64.RawURLEncoding.EncodeToString(nonce[:]), now.UnixMilli(),
		app.AndroidSigner)
	_, err = auth.Attest(t.Context(), app, proof, id, data)
	require.NoError(t, err)
	require.JSONEq(
		t, `{"integrity_token":"google-verdict"}`, string(wire.body),
	)

	var good decodeIntegrityResponse
	require.NoError(t, json.Unmarshal([]byte(wire.response), &good))
	for _, test := range []struct {
		name   string
		change func(*integrityPayload)
	}{
		{
			"certificate",
			func(v *integrityPayload) {
				v.AppIntegrity.Signers = []string{
					"foreign",
				}
			},
		},
		{
			"device",
			func(v *integrityPayload) {
				v.DeviceIntegrity.Verdicts = nil
			},
		},
		{
			"nonce",
			func(v *integrityPayload) {
				v.RequestDetails.Nonce = "other"
			},
		},
		{
			"request package",
			func(v *integrityPayload) {
				v.RequestDetails.RequestPackageName = "other"
			},
		},
		{
			"APK package",
			func(v *integrityPayload) {
				v.AppIntegrity.PackageName = "other"
			},
		},
		{
			"unevaluated",
			func(v *integrityPayload) {
				v.AppIntegrity.Recognition = "UNEVALUATED"
			},
		},
		{"stale", func(v *integrityPayload) {
			v.RequestDetails.TimestampMillis = now.
				Add(-2 * challengeLifetime).
				UnixMilli()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := good
			test.change(&bad.Payload)
			body, err := json.Marshal(bad)
			require.NoError(t, err)
			wire.response = string(body)
			_, err = auth.Attest(t.Context(), app, proof, id, data)
			require.Error(t, err)
		})
	}
}
