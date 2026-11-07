package notifyrelay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	// ErrNotFound reports missing, revoked or expired relay authority.
	ErrNotFound = errors.New("relay record not found")
	// ErrReplay reports a consumed challenge or non-increasing assertion.
	ErrReplay = errors.New("relay proof already consumed")
	// ErrCapacity asks a producer to back off without evicting valid
	// evidence.
	ErrCapacity = errors.New("relay capacity reached")
)

const (
	challengeLifetime     = 5 * time.Minute
	subscriptionLifetime  = 30 * 24 * time.Hour
	jobLifetime           = time.Hour
	dedupLifetime         = 24 * time.Hour
	claimLifetime         = 30 * time.Second
	maxConsumedChallenges = 100_000
	maxSubscriptions      = 16
	maxDedupPerRoute      = 1024
	maxDedupRecords       = 100_000
	maxPendingJobs        = 4096
)

// Bucket names belong to relay storage format version one.
const (
	installationsBucket = "installations"
	challengesBucket    = "challenges"
	subscriptionsBucket = "subscriptions"
	subscriptionIndex   = "subscription_index"
	capabilitiesBucket  = "capabilities"
	jobsBucket          = "jobs"
	dedupBucket         = "dedup"
	metadataBucket      = "metadata"
)

// installation stores public management evidence and a private APNs token.
// TokenRevision fences invalid-token responses from an older provider request.
type installation struct {
	ID            string    `json:"id"`
	Policy        AppConfig `json:"policy"`
	PublicKey     []byte    `json:"public_key"`
	Receipt       []byte    `json:"receipt"`
	Counter       uint32    `json:"counter"`
	DeviceToken   string    `json:"device_token"`
	TokenRevision uint64    `json:"token_revision"`
	TokenUpdated  int64     `json:"token_updated"`
	NextWakeAt    int64     `json:"next_wake_at"`
	NextAlertAt   int64     `json:"next_alert_at"`
	Disabled      bool      `json:"disabled"`
	Deleted       bool      `json:"deleted"`
}

// subscription binds capabilities to an app-owned route within an installation.
type subscription struct {
	ID             string `json:"id"`
	InstallationID string `json:"installation_id"`
	RouteID        string `json:"route_id"`
	WakeHash       string `json:"wake_hash"`
	AlertHash      string `json:"alert_hash"`
	ExpiresAt      int64  `json:"expires_at"`
}

// capability contains no bearer secret and grants exactly one fixed scope.
type capability struct {
	SubscriptionID string `json:"subscription_id"`
	Scope          Scope  `json:"scope"`
	ExpiresAt      int64  `json:"expires_at"`
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

// Store owns a dedicated relay database. A database file has one process owner;
// the file lock prevents multiple relay instances from silently sharing it.
type Store struct {
	db           *bolt.DB
	challengeKey [32]byte
}

// OpenStore opens or initializes durable relay format one. Callers must join
// HTTP handlers and the delivery worker before closing the returned store.
func OpenStore(path string) (*Store, error) {
	database, err := bolt.Open(path, 0600, &bolt.Options{
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	var challengeKey [32]byte
	err = database.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{
			metadataBucket, installationsBucket, challengesBucket,
			subscriptionsBucket, subscriptionIndex,
			capabilitiesBucket,
			jobsBucket, dedupBucket,
		} {
			if _, err := tx.CreateBucketIfNotExists(
				[]byte(name),
			); err != nil {
				return err
			}
		}
		meta := tx.Bucket([]byte(metadataBucket))
		version := meta.Get([]byte("format"))
		if version != nil && string(version) != "1" {
			return errors.New("unsupported notification relay " +
				"store format")
		}
		if err := meta.Put([]byte("format"), []byte("1")); err != nil {
			return err
		}
		key := meta.Get([]byte("challenge_key"))
		if key == nil {
			if _, err := rand.Read(challengeKey[:]); err != nil {
				return err
			}

			return meta.Put(
				[]byte("challenge_key"), challengeKey[:],
			)
		}
		if len(key) != len(challengeKey) {
			return errors.New("invalid relay challenge key")
		}
		copy(challengeKey[:], key)

		return nil
	})
	if err != nil {
		_ = database.Close()

		return nil, err
	}

	return &Store{db: database, challengeKey: challengeKey}, nil
}

// Close releases the file lock after all users of the store have joined.
func (s *Store) Close() error {
	return s.db.Close()
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

// secretHash is the only representation of a bearer capability stored on disk.
func secretHash(secret string) string {
	hash := sha256.Sum256([]byte(secret))

	return hex.EncodeToString(hash[:])
}

// putRecord serializes one versioned store record within its owning
// transaction.
func putRecord[T any](tx *bolt.Tx, bucket, key string, value T) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}

	return tx.Bucket([]byte(bucket)).Put([]byte(key), encoded)
}

// readRecord copies a record out of Bolt-owned memory before the view ends.
func readRecord[T any](tx *bolt.Tx, bucket, key string) (T, error) {
	var value T
	encoded := tx.Bucket([]byte(bucket)).Get([]byte(key))
	if encoded == nil {
		return value, ErrNotFound
	}
	if err := json.Unmarshal(encoded, &value); err != nil {
		return value, err
	}

	return value, nil
}

// NewChallenge issues a stateless MAC-bound nonce. Anonymous requests allocate
// no durable row; only a successfully authenticated use enters the replay set.
func (s *Store) NewChallenge(ctx context.Context, req ChallengeRequest,
	now time.Time) (Challenge, error) {

	if err := ctx.Err(); err != nil {
		return Challenge{}, err
	}
	nonce, err := randomSecret(32)
	if err != nil {
		return Challenge{}, err
	}
	value := Challenge{Nonce: nonce, AppID: req.AppID,
		Operation: req.Operation, InstallationID: req.InstallationID,
		ExpiresAt: now.Add(challengeLifetime).Unix()}
	raw, err := json.Marshal(value)
	if err != nil {
		return Challenge{}, err
	}
	mac := hmac.New(sha256.New, s.challengeKey[:])
	_, _ = mac.Write(raw)
	value.ID = base64.RawURLEncoding.EncodeToString(raw) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return value, nil
}

// loadChallenge validates a self-contained server nonce without allocating
// state. Its successful use is still checked atomically with the mutation.
func (s *Store) loadChallenge(ctx context.Context, id string, now time.Time) (
	Challenge, error) {

	if err := ctx.Err(); err != nil {
		return Challenge{}, err
	}
	if len(id) > 1024 {
		return Challenge{}, ErrNotFound
	}
	payload, signature, ok := strings.Cut(id, ".")
	if !ok {
		return Challenge{}, ErrNotFound
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Challenge{}, ErrNotFound
	}
	actual, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return Challenge{}, ErrNotFound
	}
	mac := hmac.New(sha256.New, s.challengeKey[:])
	_, _ = mac.Write(raw)
	if !hmac.Equal(actual, mac.Sum(nil)) {
		return Challenge{}, ErrNotFound
	}
	var value Challenge
	if err := json.Unmarshal(raw, &value); err != nil {
		return Challenge{}, err
	}
	if value.ID != "" || value.ExpiresAt <= now.Unix() {
		return Challenge{}, ErrNotFound
	}
	value.ID = id

	return value, nil
}

// consumeChallenge adds only an authenticated nonce to the replay set. The
// insert, assertion counter and authorized mutation commit or roll back
// together.
func consumeChallenge(tx *bolt.Tx, expected Challenge, now time.Time) error {
	if expected.ExpiresAt <= now.Unix() {
		return ErrReplay
	}
	bucket := tx.Bucket([]byte(challengesBucket))
	id := secretHash(expected.ID)
	if bucket.Get([]byte(id)) != nil {
		return ErrReplay
	}
	if bucket.Stats().KeyN >= maxConsumedChallenges {
		return ErrCapacity
	}

	return putRecord(tx, challengesBucket, id, expected.ExpiresAt)
}

// loadInstallation returns private state to the verifier or delivery owner.
func (s *Store) loadInstallation(ctx context.Context, id string) (installation,
	error) {

	var result installation
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		result, err = readRecord[installation](
			tx, installationsBucket, id,
		)
		if err != nil {
			return err
		}
		if result.Deleted {
			return ErrNotFound
		}

		return nil
	})

	return result, err
}

// enroll saves verified public key evidence and the signed token registration.
// Re-enrollment cannot replace an existing installation or reset its counter.
func (s *Store) enroll(ctx context.Context, challenge Challenge,
	value installation, now time.Time) error {

	return s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := consumeChallenge(tx, challenge, now); err != nil {
			return err
		}
		if tx.Bucket([]byte(installationsBucket)).Get(
			[]byte(value.ID),
		) != nil {
			return ErrReplay
		}

		return putRecord(tx, installationsBucket, value.ID, value)
	})
}

// applyManagement atomically consumes a nonce, advances the assertion counter
// and applies its signed action. A failed action rolls back all three.
func (s *Store) applyManagement(ctx context.Context, challenge Challenge,
	counter uint32, action ManagementPayload, now time.Time) (
	ManagementResult, error) {

	var result ManagementResult
	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		value, err := readRecord[installation](
			tx, installationsBucket, challenge.InstallationID,
		)
		if err != nil || value.Deleted {
			return ErrNotFound
		}
		if value.Policy.ID != challenge.AppID ||
			counter <= value.Counter {
			return ErrReplay
		}
		if err := consumeChallenge(tx, challenge, now); err != nil {
			return err
		}
		switch action.Action {
		case "rotate_token":
			value.DeviceToken = action.DeviceToken
			value.TokenRevision++
			value.TokenUpdated = now.UnixNano()
			value.Disabled = false

		case "subscribe":
			if value.Disabled {
				return ErrNotFound
			}
			result, err = createSubscription(
				tx, value.ID, action, now,
			)
			if err != nil {
				return err
			}

		case "revoke_subscription":
			if err := revokeSubscription(
				tx, value.ID, action.SubscriptionID,
			); err != nil {
				return err
			}

		case "delete_installation":
			value.Deleted = true
			value.Disabled = true
			value.DeviceToken = ""
			value.PublicKey = nil
			value.Receipt = nil

		default:
			return errors.New("unsupported installation action")
		}
		value.Counter = counter

		return putRecord(tx, installationsBucket, value.ID, value)
	})

	return result, err
}

// createSubscription renews one app-owned route and revokes its old capability
// hashes. Losing an HTTP reply can be recovered by a fresh attested renewal.
func createSubscription(tx *bolt.Tx, installationID string,
	action ManagementPayload, now time.Time) (ManagementResult, error) {

	var result ManagementResult
	indexKey := installationID + "/" + action.RouteID
	index := tx.Bucket([]byte(subscriptionIndex))
	var saved subscription
	if id := index.Get([]byte(indexKey)); id != nil {
		var err error
		saved, err = readRecord[subscription](
			tx, subscriptionsBucket, string(id),
		)
		if err != nil {
			return result, err
		}
		if err := deleteCapabilityHashes(tx, saved); err != nil {
			return result, err
		}
	} else {
		prefix := []byte(installationID + "/")
		cursor := index.Cursor()
		count := 0
		key, _ := cursor.Seek(prefix)
		for ; bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
			count++
		}
		if count >= maxSubscriptions {
			return result, ErrCapacity
		}
		id, err := randomSecret(16)
		if err != nil {
			return result, err
		}
		saved = subscription{
			ID:             id,
			InstallationID: installationID,
			RouteID:        action.RouteID,
		}
	}
	wake, err := randomSecret(32)
	if err != nil {
		return result, err
	}
	saved.WakeHash = secretHash(wake)
	saved.AlertHash = ""
	saved.ExpiresAt = now.Add(subscriptionLifetime).Unix()
	result = ManagementResult{
		SubscriptionID: saved.ID,
		WakeCapability: wake,
		ExpiresAt:      saved.ExpiresAt,
	}
	if err := putRecord(tx, capabilitiesBucket, saved.WakeHash, capability{
		SubscriptionID: saved.ID,
		Scope:          ScopeWake,
		ExpiresAt:      saved.ExpiresAt,
	}); err != nil {
		return ManagementResult{}, err
	}
	if action.EnableAlerts {
		alert, err := randomSecret(32)
		if err != nil {
			return ManagementResult{}, err
		}
		saved.AlertHash = secretHash(alert)
		result.AlertCapability = alert
		if err := putRecord(
			tx, capabilitiesBucket, saved.AlertHash, capability{
				SubscriptionID: saved.ID,
				Scope:          ScopeAlert,
				ExpiresAt:      saved.ExpiresAt,
			},
		); err != nil {
			return ManagementResult{}, err
		}
	}
	if err := putRecord(
		tx, subscriptionsBucket, saved.ID, saved,
	); err != nil {
		return ManagementResult{}, err
	}
	if err := index.Put([]byte(indexKey), []byte(saved.ID)); err != nil {
		return ManagementResult{}, err
	}

	return result, nil
}

// deleteCapabilityHashes revokes both permissions without retaining raw
// secrets.
func deleteCapabilityHashes(tx *bolt.Tx, value subscription) error {
	bucket := tx.Bucket([]byte(capabilitiesBucket))
	for _, hash := range []string{value.WakeHash, value.AlertHash} {
		if hash != "" {
			if err := bucket.Delete([]byte(hash)); err != nil {
				return err
			}
		}
	}

	return nil
}

// revokeSubscription checks installation ownership before removing authority.
func revokeSubscription(tx *bolt.Tx, owner, id string) error {
	value, err := readRecord[subscription](tx, subscriptionsBucket, id)
	if err != nil {
		return err
	}
	if value.InstallationID != owner {
		return ErrNotFound
	}
	if err := deleteCapabilityHashes(tx, value); err != nil {
		return err
	}
	if err := tx.Bucket([]byte(subscriptionIndex)).Delete(
		[]byte(owner + "/" + value.RouteID),
	); err != nil {
		return err
	}

	return tx.Bucket([]byte(subscriptionsBucket)).Delete([]byte(id))
}

// authorizedSubscription resolves one live capability through its fixed
// subscription and installation. Admission and non-sending verification share
// this boundary so a revoked, expired or disabled grant cannot pass either.
func authorizedSubscription(tx *bolt.Tx, secret string, scope Scope,
	now time.Time) (subscription, installation, error) {

	var empty subscription
	var noOwner installation
	authority, err := readRecord[capability](
		tx, capabilitiesBucket, secretHash(secret),
	)
	if err != nil || authority.Scope != scope ||
		authority.ExpiresAt <= now.Unix() {
		return empty, noOwner, ErrNotFound
	}
	sub, err := readRecord[subscription](
		tx, subscriptionsBucket, authority.SubscriptionID,
	)
	if err != nil || sub.ExpiresAt <= now.Unix() {
		return empty, noOwner, ErrNotFound
	}
	owner, err := readRecord[installation](
		tx, installationsBucket, sub.InstallationID,
	)
	if err != nil || owner.Deleted || owner.Disabled {
		return empty, noOwner, ErrNotFound
	}

	return sub, owner, nil
}

// verifySubscription proves grant issuance without allocating a job or calling
// APNs. A mailbox key alone must not let a producer store arbitrary fake grants
// as if they came from an attested app. The caller checks publisher policy.
func (s *Store) verifySubscription(ctx context.Context, wake string,
	request SubscriptionVerificationRequest, now time.Time) (AppConfig,
	int64, error) {

	var policy AppConfig
	var expiry int64
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		sub, owner, err := authorizedSubscription(
			tx, wake, ScopeWake, now,
		)
		if err != nil || sub.ID != request.SubscriptionID {
			return ErrNotFound
		}
		if request.AlertCapability != "" {
			alert, _, err := authorizedSubscription(
				tx, request.AlertCapability, ScopeAlert, now,
			)
			if err != nil || alert.ID != sub.ID {
				return ErrNotFound
			}
		}
		policy = owner.Policy
		expiry = sub.ExpiresAt

		return nil
	})

	return policy, expiry, err
}

// enqueue authenticates a fixed-scope bearer capability and commits dedup and
// coalesced work together. The request cannot provide a destination or
// template.
func (s *Store) enqueue(ctx context.Context, secret string, scope Scope,
	eventID string, now time.Time) error {

	return s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		sub, _, err := authorizedSubscription(tx, secret, scope, now)
		if err != nil {
			return err
		}
		dedupID := sub.ID + "/" + string(scope) + "/" + secretHash(
			eventID,
		)
		dedup := tx.Bucket([]byte(dedupBucket))
		if raw := dedup.Get([]byte(dedupID)); raw != nil {
			var expiry int64
			if err := json.Unmarshal(raw, &expiry); err != nil {
				return err
			}
			if expiry > now.Unix() {
				return nil
			}
		}
		if dedup.Stats().KeyN >= maxDedupRecords {
			return ErrCapacity
		}
		prefix := []byte(sub.ID + "/")
		count := 0
		cursor := dedup.Cursor()
		key, _ := cursor.Seek(prefix)
		for ; bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
			count++
		}
		if count >= maxDedupPerRoute {
			return ErrCapacity
		}
		jobID := string(scope) + "/" + sub.ID
		job, err := readRecord[relayJob](tx, jobsBucket, jobID)
		if errors.Is(err, ErrNotFound) {
			jobs := tx.Bucket([]byte(jobsBucket))
			if jobs.Stats().KeyN >= maxPendingJobs {
				return ErrCapacity
			}
			job = relayJob{
				ID:             jobID,
				SubscriptionID: sub.ID,
				Scope:          scope,
				NextAt:         now.UnixNano(),
			}
		} else if err != nil {
			return err
		}
		job.Generation++
		job.EventHash = secretHash(eventID)
		job.ExpiresAt = now.Add(jobLifetime).UnixNano()
		if err := putRecord(tx, jobsBucket, job.ID, job); err != nil {
			return err
		}

		return putRecord(
			tx, dedupBucket, dedupID, now.Add(dedupLifetime).Unix(),
		)
	})
}
