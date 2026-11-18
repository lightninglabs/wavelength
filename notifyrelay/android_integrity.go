package notifyrelay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"time"
)

// decodeIntegrityRequest submits the opaque Play Integrity token from the APK.
type decodeIntegrityRequest struct {
	// IntegrityToken is the signed and encrypted classic token issued by
	// Google.
	IntegrityToken string `json:"integrity_token"`
}

type decodeIntegrityResponse struct {
	// Payload is Google's decoded and verified verdict.
	//nolint:tagliatelle
	Payload integrityPayload `json:"tokenPayloadExternal"`
}

// integrityPayload contains the parts of Google's verdict used for admission.
//
//nolint:tagliatelle
type integrityPayload struct {
	// RequestDetails binds the verdict to a package, challenge and time.
	RequestDetails integrityRequestDetails `json:"requestDetails"`
	// AppIntegrity identifies the APK and its signing certificates.
	AppIntegrity integrityApplication `json:"appIntegrity"`
	// DeviceIntegrity reports the integrity labels the device earned.
	DeviceIntegrity integrityDevice `json:"deviceIntegrity"`
}

//nolint:tagliatelle
type integrityRequestDetails struct {
	// RequestPackageName is the package that requested the verdict.
	RequestPackageName string `json:"requestPackageName"`
	// Nonce binds the installation key to the relay enrollment transcript.
	Nonce string `json:"nonce"`
	// TimestampMillis is the request time in Unix milliseconds, encoded as
	// a string.
	TimestampMillis int64 `json:"timestampMillis,string"`
}

//nolint:tagliatelle
type integrityApplication struct {
	// Recognition indicates whether Google evaluated the APK.
	Recognition string `json:"appRecognitionVerdict"`
	// PackageName identifies the evaluated APK.
	PackageName string `json:"packageName"`
	// Signers contains base64url-encoded SHA-256 certificate digests.
	Signers []string `json:"certificateSha256Digest"`
}

//nolint:tagliatelle
type integrityDevice struct {
	// Verdicts contains the device recognition labels issued by Google.
	Verdicts []string `json:"deviceRecognitionVerdict"`
}

// checkIntegrity asks Google to decode the verdict for the publisher's
// package. Direct APKs can be unrecognized by Play. Both distribution paths
// require an evaluated package and exact signer on a certified device.
func (a *AndroidAuthenticator) checkIntegrity(ctx context.Context,
	app AppConfig, keyID string, data []byte, token string) error {

	invalid := errors.New("invalid Android integrity proof")
	if a.client == nil || len(token) == 0 || len(token) > 32000 {
		return invalid
	}
	body, err := json.Marshal(decodeIntegrityRequest{IntegrityToken: token})
	if err != nil {
		return invalid
	}
	url := "https://playintegrity.googleapis.com/v1/" + app.AndroidPackage +
		":decodeIntegrityToken"
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, url, bytes.NewReader(body),
	)
	if err != nil {
		return invalid
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return invalid
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return invalid
	}
	var verdict decodeIntegrityResponse
	if json.NewDecoder(io.LimitReader(resp.Body, 32*1024)).Decode(
		&verdict,
	) != nil {
		return invalid
	}
	v := verdict.Payload
	// Include the key ID: the canonical enrollment transcript binds the
	// destination, but intentionally has no credential ID.
	hash := sha256.Sum256(append([]byte(keyID+"\n"), data...))
	nonce := base64.RawURLEncoding.EncodeToString(hash[:])
	age := time.Since(time.UnixMilli(v.RequestDetails.TimestampMillis))
	appVerdict := v.AppIntegrity.Recognition
	if v.RequestDetails.RequestPackageName != app.AndroidPackage ||
		v.RequestDetails.Nonce != nonce || age < -time.Minute ||
		age > challengeLifetime ||
		(appVerdict != "PLAY_RECOGNIZED" &&
			appVerdict != "UNRECOGNIZED_VERSION") ||
		v.AppIntegrity.PackageName != app.AndroidPackage ||
		!slices.Equal(
			v.AppIntegrity.Signers, []string{app.AndroidSigner},
		) ||
		!slices.Contains(
			v.DeviceIntegrity.Verdicts, "MEETS_DEVICE_INTEGRITY",
		) {
		return invalid
	}

	return nil
}
