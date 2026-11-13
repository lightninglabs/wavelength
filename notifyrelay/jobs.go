package notifyrelay

import (
	"context"
	"fmt"
	"time"
)

const (
	// Successful silent pushes observe Apple's low-frequency background
	// delivery guidance. Alerts have an independent per-installation
	// budget.
	wakeQuietPeriod  = 20 * time.Minute
	alertQuietPeriod = time.Minute
	// Signing phases can span several host execution windows. Coalesce
	// each device's round traffic without imposing the maintenance budget.
	roundQuietPeriod = 5 * time.Second
	// Scheduled maintenance remains low frequency even on dense timetables.
	scheduleQuietPeriod = 20 * time.Minute
)

// Outcome describes a provider result without exposing credentials or tokens.
type Outcome uint8

const (
	// OutcomeRetry retains the same durable job after a temporary failure.
	OutcomeRetry Outcome = iota
	// OutcomeAccepted means APNs accepted the request, not that it
	// displayed.
	OutcomeAccepted
	// OutcomeInvalidToken permits revision-fenced installation
	// invalidation.
	OutcomeInvalidToken
	// OutcomeRejected retires a malformed request that retry cannot repair.
	OutcomeRejected
)

// ProviderResult separates permanent token invalidation from transient errors.
// InvalidAt is APNs' invalidation timestamp when one was supplied.
type ProviderResult struct {
	Outcome    Outcome
	InvalidAt  time.Time
	RetryAfter time.Duration
}

// Delivery is a frozen provider request. DeviceToken is private routing data;
// String deliberately excludes it, and JSON serialization never emits it.
type Delivery struct {
	DeviceToken  string            `json:"-"`
	Topic        string            `json:"topic"`
	Environment  string            `json:"environment"`
	Scope        Scope             `json:"scope"`
	Presentation AlertPresentation `json:"alert_presentation,omitempty"`
	RouteID      string            `json:"route_id"`
	EventHash    string            `json:"event_hash"`
	CollapseID   string            `json:"collapse_id"`
	ExpiresAt    time.Time         `json:"expires_at"`

	installationID string
	job            relayJob
	tokenRevision  uint64
	policy         AppConfig
}

// String makes routine diagnostics safe even when a delivery is formatted.
func (d Delivery) String() string {
	return fmt.Sprintf("notification delivery (%s)", d.Scope)
}

// Provider sends only a server-constructed, fixed-template delivery. Errors
// must not contain device tokens, provider JWTs or request URLs containing
// them.
type Provider interface {
	// ValidateApplication checks the provider-specific destination policy.
	ValidateApplication(AppConfig) error

	// NormalizeDeviceToken validates and canonicalizes a provider
	// destination. Opaque case-sensitive tokens must not inherit APNs hex
	// normalization.
	NormalizeDeviceToken(string) (string, error)

	// Send observes its context and returns provider acceptance separately
	// from eventual device delivery. Implementations must not retarget a
	// delivery.
	Send(context.Context, Delivery) (ProviderResult, error)
}
