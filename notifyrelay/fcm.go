package notifyrelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// FCMProvider delivers data-only hints to a Firebase installation. The client
// obtains publisher credentials at send time using Google's OAuth transport.
type FCMProvider struct {
	client http.Client
}

// fcmErrorType identifies the typed FCM detail in a Google RPC error.
const fcmErrorType = "type.googleapis.com/google.firebase.fcm.v1.FcmError"

// firebaseProjectID matches project IDs accepted in the FCM endpoint path.
var firebaseProjectID = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

// firebaseInstallationID matches the case-sensitive, 22-character FID.
var firebaseInstallationID = regexp.MustCompile(`^[cdef][A-Za-z0-9_-]{21}$`)

// fcmSendRequest is the HTTP v1 projects.messages.send request body.
type fcmSendRequest struct {
	// Message carries one data-only delivery.
	Message fcmMessage `json:"message"`
}

type fcmMessage struct {
	// FID addresses the installation registered by the Android app.
	FID string `json:"fid"`
	// Data carries the app's string-valued hint payload.
	Data fcmHint `json:"data"`
	// Android sets package restrictions and delivery options.
	Android fcmAndroidConfig `json:"android"`
}

type fcmHint struct {
	// Version identifies the data schema understood by the receiver.
	Version string `json:"version"`
	// RouteID selects the locally authorized wallet binding.
	RouteID string `json:"route_id"`
	// Kind distinguishes a payment activity hint from a wake.
	Kind string `json:"kind"`
	// EventID identifies the event to check against local payment receipts.
	EventID string `json:"event_id,omitempty"`
	// WakeReason identifies the work requested by a wake.
	WakeReason string `json:"wake_reason,omitempty"`
	// WakeDeadlineUnix is the producer deadline in decimal Unix seconds.
	WakeDeadlineUnix string `json:"wake_deadline_unix,omitempty"`
}

type fcmAndroidConfig struct {
	// Priority controls when Android may deliver the message.
	Priority string `json:"priority"`
	// TTL limits how long FCM may retain an undelivered hint.
	TTL string `json:"ttl"`
	// RestrictedPackageName limits delivery to the publisher's APK.
	RestrictedPackageName string `json:"restricted_package_name"`
}

type fcmErrorResponse struct {
	// Error carries the Google RPC status returned for a failed send.
	Error fcmStatus `json:"error"`
}

type fcmStatus struct {
	// Details contains typed extensions to the Google RPC status.
	Details []fcmErrorDetail `json:"details"`
}

type fcmErrorDetail struct {
	// Type is the protobuf type URL of this detail.
	Type string `json:"@type"`
	// Code is the provider reason when Type identifies an FcmError.
	//nolint:tagliatelle
	Code string `json:"errorCode"`
}

// NewFCMProvider fixes the service origin and disables redirects so routing
// data and the authenticated request cannot travel to another host.
func NewFCMProvider(transport http.RoundTripper) *FCMProvider {
	return &FCMProvider{client: http.Client{
		Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// ValidateApplication checks immutable Android package and Firebase routing
// policy before the server starts consuming jobs.
func (p *FCMProvider) ValidateApplication(app AppConfig) error {
	if !firebaseProjectID.MatchString(app.FirebaseProject) ||
		!validIdentifier(app.AndroidPackage, 255) {
		return errors.New("invalid Firebase project or Android package")
	}

	return nil
}

// NormalizeDeviceToken accepts current case-sensitive Firebase installation
// IDs.
func (p *FCMProvider) NormalizeDeviceToken(token string) (string, error) {
	if !firebaseInstallationID.MatchString(token) {
		return "", errors.New("invalid Firebase installation ID")
	}

	return token, nil
}

// Send targets the frozen installation and never requests OS presentation.
// Only an explicit FCM unregistration permits SQL to invalidate a destination.
func (p *FCMProvider) Send(ctx context.Context, d Delivery) (ProviderResult,
	error) {

	reject := ProviderResult{Outcome: OutcomeRejected}
	retry := ProviderResult{Outcome: OutcomeRetry}
	if err := p.ValidateApplication(d.policy); err != nil {
		return reject, err
	}
	if _, err := p.NormalizeDeviceToken(d.DeviceToken); err != nil {
		return reject, err
	}
	data := fcmHint{Version: "1", RouteID: d.RouteID}
	switch d.Scope {
	case ScopeAlert:
		data.Kind = "payment_activity"
		data.EventID = d.EventHash

	case ScopeWake:
		data.Kind = "wake"
		switch d.job.Scope {
		case ScopeSchedule:
			data.WakeReason = "scheduled_round"

		case ScopeRound:
			data.WakeReason = "round_signing"

		case ScopeWake:
			data.WakeReason = "mailbox_update"

		case ScopeAlert:
			return reject, errors.New("invalid frozen wake scope")
		}
		if d.job.WakeDeadlineUnix > 0 {
			data.WakeDeadlineUnix = strconv.FormatInt(
				d.job.WakeDeadlineUnix, 10,
			)
		}

	default:
		return reject, errors.New("unknown FCM delivery scope")
	}
	ttl := int64(time.Until(d.ExpiresAt) / time.Second)
	if ttl < 0 {
		return reject, nil
	}
	body, err := json.Marshal(fcmSendRequest{Message: fcmMessage{
		FID:  d.DeviceToken,
		Data: data,
		Android: fcmAndroidConfig{
			Priority: "normal",
			TTL: strconv.FormatInt(
				min(ttl, 2419200), 10,
			) + "s",
			RestrictedPackageName: d.policy.AndroidPackage,
		},
	}})
	if err != nil {
		return reject, err
	}
	url := "https://fcm.googleapis.com/v1/projects/" +
		d.policy.FirebaseProject + "/messages:send"
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, url, bytes.NewReader(body),
	)
	if err != nil {
		return reject, errors.New("construct FCM request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return retry, errors.New("FCM transport unavailable")
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusOK {
		return ProviderResult{Outcome: OutcomeAccepted}, nil
	}
	var failure fcmErrorResponse
	_ = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&failure)
	sendErr := fmt.Errorf("FCM send returned HTTP %d", resp.StatusCode)
	for _, detail := range failure.Error.Details {
		if detail.Type != fcmErrorType {
			continue
		}
		if resp.StatusCode == http.StatusNotFound &&
			detail.Code == "UNREGISTERED" {
			return ProviderResult{Outcome: OutcomeInvalidToken}, nil
		}
		// Only log documented reason codes. A provider response may
		// contain destination data elsewhere in its error body.
		switch detail.Code {
		case "UNSPECIFIED_ERROR", "INVALID_ARGUMENT", "UNREGISTERED",
			"SENDER_ID_MISMATCH", "QUOTA_EXCEEDED", "UNAVAILABLE",
			"INTERNAL", "THIRD_PARTY_AUTH_ERROR":

			sendErr = fmt.Errorf("%w (%s)", sendErr, detail.Code)
		}

		break
	}
	if resp.StatusCode == http.StatusBadRequest {
		return reject, sendErr
	}
	if secs, err := strconv.ParseUint(
		resp.Header.Get("Retry-After"), 10, 32,
	); err == nil {

		retry.RetryAfter = time.Duration(secs) * time.Second
	}

	return retry, sendErr
}
