// Package notifyrelay provides a publisher-owned notification relay. App
// Attest authorizes installation management; scoped capabilities authorize
// fixed-destination, fixed-template notifications without wallet credentials.
package notifyrelay

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Operation names bind each App Attest proof to one management action.
const (
	OperationEnroll = "enroll"
	OperationManage = "manage"
)

// Scope identifies the only two notification permissions a relay can issue.
type Scope string

const (
	// ScopeWake permits a silent request to reconcile local wallet state.
	ScopeWake Scope = "wake"
	// ScopeAlert permits an authorized incoming-payment notification.
	ScopeAlert Scope = "alert"
)

// AlertPresentation selects the app-authorized treatment of payment events.
// It never changes the producer's authority or permits custom push contents.
type AlertPresentation string

const (
	// PresentationGeneric sends the fixed visible payment-activity alert.
	PresentationGeneric AlertPresentation = "generic"
	// PresentationReceiptHint sends a silent payment hint for local
	// verification.
	PresentationReceiptHint AlertPresentation = "receipt_hint"
)

// normalized preserves the visible template for older clients and stored rows.
func (p AlertPresentation) normalized() AlertPresentation {
	if p == "" {
		return PresentationGeneric
	}

	return p
}

// ChallengeRequest selects a server-configured app and management operation.
// InstallationID is required only for management of an existing installation.
type ChallengeRequest struct {
	AppID          string `json:"app_id"`
	Operation      string `json:"operation"`
	InstallationID string `json:"installation_id,omitempty"`
}

// Challenge is a one-use server nonce. AppID is the relay's configured app
// alias, not an arbitrary Apple audience supplied by a caller.
type Challenge struct {
	ID             string `json:"id"`
	Nonce          string `json:"nonce"`
	AppID          string `json:"app_id"`
	Operation      string `json:"operation"`
	InstallationID string `json:"installation_id,omitempty"`
	ExpiresAt      int64  `json:"expires_at"`
}

// ProofRequest carries an attestation or assertion and the exact JSON payload
// bytes it signs. Byte slices use standard padded base64 in JSON. KeyID uses
// DCAppAttestService's standard base64 key identifier and is enrollment-only.
type ProofRequest struct {
	ChallengeID string `json:"challenge_id"`
	KeyID       string `json:"key_id,omitempty"`
	Proof       []byte `json:"proof"`
	Payload     []byte `json:"payload"`
}

// EnrollmentPayload binds initial token registration to the attested key.
// Topic, APNs environment and Apple App ID are selected by server policy.
type EnrollmentPayload struct {
	DeviceToken string `json:"device_token"`
}

// EnrollmentResult identifies an installation without exposing its device
// token, attestation receipt or private management authority.
type EnrollmentResult struct {
	InstallationID string `json:"installation_id"`
}

// ManagementPayload is signed in full by an installation's App Attest key.
// Action is rotate_token, subscribe, revoke_subscription or
// delete_installation. RouteID is an opaque app-owned wallet/network lookup
// key, never a URL.
type ManagementPayload struct {
	Action         string            `json:"action"`
	DeviceToken    string            `json:"device_token,omitempty"`
	RouteID        string            `json:"route_id,omitempty"`
	EnableAlerts   bool              `json:"enable_alerts,omitempty"`
	Presentation   AlertPresentation `json:"alert_presentation,omitempty"`
	SubscriptionID string            `json:"subscription_id,omitempty"`
}

// ManagementResult returns capabilities only when a subscription is created
// or renewed. Capabilities must be stored privately and never logged.
type ManagementResult struct {
	SubscriptionID  string            `json:"subscription_id,omitempty"`
	WakeCapability  string            `json:"wake_capability,omitempty"`
	AlertCapability string            `json:"alert_capability,omitempty"`
	ExpiresAt       int64             `json:"expires_at,omitempty"`
	Presentation    AlertPresentation `json:"alert_presentation,omitempty"`
}

// PushRequest contains an opaque producer-owned idempotency identity only.
// A bearer capability fixes the destination, routing key, scope and template.
type PushRequest struct {
	EventID string `json:"event_id"`
}

// SubscriptionVerificationRequest lets a producer prove that wake and optional
// alert capabilities were issued for the same live subscription. It is not an
// enrollment or push operation. The wake capability is supplied as a bearer.
type SubscriptionVerificationRequest struct {
	SubscriptionID  string `json:"subscription_id"`
	AlertCapability string `json:"alert_capability,omitempty"`
}

// SubscriptionVerificationResult discloses expiry only. It never returns an
// installation identity, route, device token or other private management data.
type SubscriptionVerificationResult struct {
	ExpiresAt int64 `json:"expires_at"`
}

// ClientData constructs the exact App Attest transcript used by both the app
// and relay. Fields are ASCII identifiers issued or validated by the relay.
// The final newline is significant. Payload bytes are hashed without parsing
// or re-encoding, avoiding cross-language JSON canonicalization differences.
func ClientData(challenge Challenge, payload []byte) []byte {
	digest := sha256.Sum256(payload)
	fields := []string{
		"wavelength.notification-relay.v1", challenge.Operation,
		challenge.AppID, challenge.InstallationID, challenge.ID,
		challenge.Nonce, hex.EncodeToString(digest[:]), "",
	}

	return []byte(strings.Join(fields, "\n"))
}
