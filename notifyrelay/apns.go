package notifyrelay

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// APNSConfig holds publisher credentials. Load PrivateKey from a mounted
// secret; never log this configuration or accept it from an HTTP caller.
type APNSConfig struct {
	TeamID     string
	KeyID      string
	PrivateKey *ecdsa.PrivateKey
	HTTPClient *http.Client
}

// APNSProvider signs provider JWTs and sends HTTP/2 requests to Apple's fixed
// production/development endpoints. It does not perform relay authorization.
type APNSProvider struct {
	cfg    APNSConfig
	now    func() time.Time
	mu     sync.Mutex
	token  string
	issued time.Time
}

// ParseAPNSKey accepts Apple's PKCS#8 P-256 provider key format only.
func ParseAPNSKey(raw []byte) (*ecdsa.PrivateKey, error) {
	block, remainder := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" ||
		len(bytes.TrimSpace(remainder)) != 0 {
		return nil, errors.New("APNs key must contain one PKCS#8 " +
			"private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid APNs private key")
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("APNs key must use P-256")
	}

	return key, nil
}

// NewAPNSProvider validates publisher credentials before any request is sent.
// A supplied HTTPClient is useful for tests; destinations remain fixed by Send.
func NewAPNSProvider(cfg APNSConfig) (*APNSProvider, error) {
	if !validIdentifier(cfg.TeamID, 10) || len(cfg.TeamID) != 10 ||
		!validIdentifier(cfg.KeyID, 10) || len(cfg.KeyID) != 10 ||
		cfg.PrivateKey == nil ||
		cfg.PrivateKey.Curve != elliptic.P256() {
		return nil, errors.New("APNs team, key identifier and P-256 " +
			"key required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				Proxy:             http.ProxyFromEnvironment,
				ForceAttemptHTTP2: true,
				TLSClientConfig: &tls.Config{
					MinVersion: tls.VersionTLS12,
				},
				TLSHandshakeTimeout:   5 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				MaxIdleConns:          10,
				MaxIdleConnsPerHost:   10,
			},
		}
	}

	// Never follow a redirect away from the fixed APNs provider endpoint.
	// Clone a supplied client so this policy does not mutate its owner's
	// client.
	client := *cfg.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	cfg.HTTPClient = &client

	return &APNSProvider{cfg: cfg, now: time.Now}, nil
}

// providerToken returns a cached ES256 JWT, refreshing well before Apple's
// one-hour token limit. This cache contains a credential and is never logged.
func (p *APNSProvider) providerToken() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if p.token != "" && now.Sub(p.issued) >= 0 &&
		now.Sub(p.issued) < 20*time.Minute {
		return p.token, nil
	}
	header, err := json.Marshal(map[string]string{
		"alg": "ES256", "kid": p.cfg.KeyID,
	})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(struct {
		Issuer string `json:"iss"`
		Issued int64  `json:"iat"`
	}{Issuer: p.cfg.TeamID, Issued: now.Unix()})
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(encoded))
	r, s, err := ecdsa.Sign(rand.Reader, p.cfg.PrivateKey, digest[:])
	if err != nil {
		return "", err
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	p.token = encoded + "." + base64.RawURLEncoding.EncodeToString(
		signature,
	)
	p.issued = now

	return p.token, nil
}

// invalidateProviderToken ignores an authorization failure for an older JWT
// after another request has already refreshed the shared token.
func (p *APNSProvider) invalidateProviderToken(sent string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token == sent {
		p.token = ""
	}
}

// validDeviceToken accepts variable-length hexadecimal tokens. Apple's token
// length is not a protocol constant; the upper bound is a local input budget.
func validDeviceToken(token string) bool {
	if token == "" || len(token) > 512 {
		return false
	}
	_, err := hex.DecodeString(token)

	return err == nil
}

// notificationPayload is the complete fixed push format. Route and event hash
// are lookup hints only; the app must reconcile and validate wallet activity.
func notificationPayload(delivery Delivery) ([]byte, error) {
	aps := map[string]any{"content-available": 1}
	if delivery.Scope == ScopeAlert {
		aps["alert"] = map[string]string{
			"title": "Wavelength",
			"body": "Payment activity. Open Wavelength " +
				"to update your wallet.",
		}
	} else if delivery.Scope != ScopeWake {
		return nil, errors.New("unknown notification scope")
	}

	return json.Marshal(map[string]any{
		"aps": aps,
		"wavelength": map[string]any{
			"version": 1, "route_id": delivery.RouteID,
			"event_id": delivery.EventHash,
		},
	})
}

// providerRequestID is stable across retries of one claimed generation, while
// a new event produces a new identifier. It grants no notification authority.
func providerRequestID(delivery Delivery) string {
	value := delivery.job.ID + "/" + delivery.EventHash + "/" +
		strconv.FormatUint(delivery.job.Generation, 10)
	hash := sha256.Sum256([]byte(value))
	hash[6] = hash[6]&0x0f | 0x40
	hash[8] = hash[8]&0x3f | 0x80

	return uuid.UUID(hash[:16]).String()
}

// Send posts a fixed payload to APNs. Transport errors are deliberately
// sanitized: net/http errors include the URL, whose path contains the token.
func (p *APNSProvider) Send(ctx context.Context, delivery Delivery) (
	ProviderResult, error) {

	retry := ProviderResult{Outcome: OutcomeRetry}
	if !validDeviceToken(delivery.DeviceToken) ||
		!validIdentifier(delivery.Topic, 255) {
		return ProviderResult{Outcome: OutcomeRejected},
			errors.New("invalid stored APNs destination")
	}
	host := "https://api.push.apple.com"
	if delivery.Environment == "development" {
		host = "https://api.sandbox.push.apple.com"
	} else if delivery.Environment != "production" {
		return ProviderResult{Outcome: OutcomeRejected},
			errors.New("invalid stored APNs environment")
	}
	payload, err := notificationPayload(delivery)
	if err != nil {
		return ProviderResult{Outcome: OutcomeRejected}, err
	}
	token, err := p.providerToken()
	if err != nil {
		return retry, errors.New("APNs provider token signing failed")
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, host+"/3/device/"+delivery.DeviceToken,
		bytes.NewReader(payload),
	)
	if err != nil {
		return retry, errors.New("APNs request construction failed")
	}
	pushType := "background"
	if delivery.Scope == ScopeAlert {
		pushType = "alert"
	}
	request.Header.Set("Authorization", "bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Apns-Topic", delivery.Topic)
	request.Header.Set("Apns-Push-Type", pushType)
	request.Header.Set("Apns-Priority", "5")
	request.Header.Set("Apns-Collapse-Id", delivery.CollapseID)
	request.Header.Set("Apns-Id", providerRequestID(delivery))
	request.Header.Set(
		"Apns-Expiration",
		strconv.FormatInt(
			delivery.ExpiresAt.Unix(), 10,
		),
	)
	response, err := p.cfg.HTTPClient.Do(request)
	if err != nil {
		return retry, errors.New("APNs transport unavailable")
	}
	defer func() {
		_ = response.Body.Close()
	}()
	if response.StatusCode == http.StatusOK {
		return ProviderResult{Outcome: OutcomeAccepted}, nil
	}
	var failure struct {
		Reason    string `json:"reason"`
		Timestamp int64  `json:"timestamp"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(
		&failure,
	)
	if response.StatusCode == http.StatusGone ||
		response.StatusCode == http.StatusBadRequest &&
			(failure.Reason == "BadDeviceToken" ||
				failure.Reason == "DeviceTokenNotForTopic") {

		result := ProviderResult{Outcome: OutcomeInvalidToken}
		if failure.Timestamp > 0 {
			result.InvalidAt = time.UnixMilli(failure.Timestamp)
		}

		return result, nil
	}
	if response.StatusCode == http.StatusForbidden {
		p.invalidateProviderToken(token)
	}
	if response.StatusCode >= 500 ||
		response.StatusCode == http.StatusTooManyRequests ||
		response.StatusCode == http.StatusForbidden {

		seconds, _ := strconv.ParseUint(
			strings.TrimSpace(
				response.Header.Get("Retry-After"),
			),
			10,
			32,
		)
		retry.RetryAfter = time.Duration(seconds) * time.Second

		return retry, fmt.Errorf("APNs temporary failure (status %d)",
			response.StatusCode)
	}

	return ProviderResult{Outcome: OutcomeRejected},
		fmt.Errorf("APNs request rejected (status %d)",
			response.StatusCode)
}
