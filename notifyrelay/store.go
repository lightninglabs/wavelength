package notifyrelay

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"
)

var (
	// ErrNotFound reports missing, revoked or expired relay authority.
	ErrNotFound = errors.New("relay record not found")
	// ErrReplay reports a consumed challenge or non-increasing assertion.
	ErrReplay = errors.New("relay proof already consumed")
	// ErrCapacity applies bounded backpressure without evicting evidence.
	ErrCapacity = errors.New("relay capacity reached")
)

const (
	challengeLifetime       = 5 * time.Minute
	subscriptionLifetime    = 30 * 24 * time.Hour
	jobLifetime             = time.Hour
	dedupLifetime           = 24 * time.Hour
	claimLifetime           = 30 * time.Second
	maxConsumedChallenges   = 100_000
	maxSubscriptions        = 16
	maxDedupPerRoute        = 1024
	maxDedupPerInstallation = 1024
	maxDedupRecords         = 100_000
	// Existing wake/alert work shares one pool. Each new round lane has
	// its own equally bounded pool so future schedules cannot block
	// signing or payment delivery. Total job storage is at most 3x this.
	maxPendingJobs = 4096
)

// installation stores public management evidence and a private APNs token.
// TokenRevision fences invalid-token responses from an older provider request.
type installation struct {
	ID             string    `json:"id"`
	Policy         AppConfig `json:"policy"`
	PublicKey      []byte    `json:"public_key"`
	Receipt        []byte    `json:"receipt"`
	Counter        uint32    `json:"counter"`
	DeviceToken    string    `json:"device_token"`
	TokenRevision  uint64    `json:"token_revision"`
	TokenUpdated   int64     `json:"token_updated"`
	NextWakeAt     int64     `json:"next_wake_at"`
	NextRoundAt    int64     `json:"next_round_at"`
	NextScheduleAt int64     `json:"next_schedule_at"`
	NextAlertAt    int64     `json:"next_alert_at"`
	Disabled       bool      `json:"disabled"`
	Deleted        bool      `json:"deleted"`
}

// relayJob coalesces new work while a provider request is in flight. Completing
// generation N cannot erase a newer N+1 admitted during that request.
type relayJob struct {
	ID             string `json:"id"`
	SubscriptionID string `json:"subscription_id"`
	Scope          Scope  `json:"scope"`
	Generation     uint64 `json:"generation"`
	DoneGeneration uint64 `json:"done_generation"`
	EventHash      string `json:"event_hash"`
	NextAt         int64  `json:"next_at"`
	ExpiresAt      int64  `json:"expires_at"`
	ClaimToken     string `json:"claim_token"`
	LeaseUntil     int64  `json:"lease_until"`
	Attempts       uint32 `json:"attempts"`
}

// InstallationID derives the public routing identity before enrollment, so a
// client can recover an ambiguous enrollment reply using a signed assertion.
func InstallationID(appAlias, keyID string) (string, error) {
	key, err := base64.StdEncoding.DecodeString(keyID)
	if err != nil || len(key) != sha256.Size ||
		!validIdentifier(appAlias, 100) {
		return "", errors.New("invalid installation identity")
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(appAlias + "\x00"))
	_, _ = hash.Write(key)

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// randomSecret creates an unguessable identifier without printable private
// data.
func randomSecret(byteCount int) (string, error) {
	buffer := make([]byte, byteCount)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// secretHash keeps opaque correlation identities fixed-size in replay storage.
func secretHash(secret string) string {
	hash := sha256.Sum256([]byte(secret))

	return hex.EncodeToString(hash[:])
}
