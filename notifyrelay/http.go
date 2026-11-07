package notifyrelay

import (
	"bytes"
	"net/http"
	"strings"
)

// challenge validates the configured app/action before allocating nonce state.
func (s *Server) challenge(w http.ResponseWriter, r *http.Request) {
	var req ChallengeRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")

		return
	}
	app, ok := s.apps[req.AppID]
	if !ok {
		writeError(w, http.StatusUnauthorized, "unsupported_app")

		return
	}
	switch req.Operation {
	case OperationEnroll:
		if req.InstallationID != "" {
			writeError(w, http.StatusBadRequest, "invalid_request")

			return
		}

	case OperationManage:
		owner, err := s.store.loadInstallation(
			r.Context(), req.InstallationID,
		)
		if err != nil || owner.Policy != app {
			writeError(w, http.StatusUnauthorized, "unauthorized")

			return
		}

	default:
		writeError(w, http.StatusBadRequest, "invalid_operation")

		return
	}
	value, err := s.store.NewChallenge(r.Context(), req, s.now())
	if err != nil {
		storeError(w, err)

		return
	}
	writeJSON(w, http.StatusOK, value)
}

// enroll verifies Apple's key attestation bound to the exact token payload.
// Its final transaction consumes the nonce with the installation insert.
func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	var proof ProofRequest
	if err := decodeJSON(r.Body, &proof); err != nil ||
		len(proof.Proof) == 0 {

		writeError(w, http.StatusBadRequest, "invalid_request")

		return
	}
	challenge, err := s.store.loadChallenge(
		r.Context(), proof.ChallengeID, s.now(),
	)
	if err != nil || challenge.Operation != OperationEnroll {
		writeError(w, http.StatusUnauthorized, "invalid_proof")

		return
	}
	app, ok := s.apps[challenge.AppID]
	if !ok {
		writeError(w, http.StatusUnauthorized, "unsupported_app")

		return
	}
	var payload EnrollmentPayload
	if err := decodeJSON(
		bytes.NewReader(proof.Payload), &payload,
	); err != nil ||
		!validDeviceToken(payload.DeviceToken) {

		writeError(w, http.StatusBadRequest, "invalid_payload")

		return
	}
	id, err := InstallationID(challenge.AppID, proof.KeyID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_key")

		return
	}
	key, err := s.verifier.attest(
		app, proof.Proof, proof.KeyID,
		ClientData(challenge, proof.Payload),
	)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_proof")

		return
	}
	now := s.now()
	err = s.store.enroll(r.Context(), challenge, installation{
		ID: id, Policy: app,
		PublicKey: key.publicKey, Receipt: key.receipt,
		DeviceToken:   strings.ToLower(payload.DeviceToken),
		TokenRevision: 1, TokenUpdated: now.UnixNano(),
	}, now)
	if err != nil {
		storeError(w, err)

		return
	}
	writeJSON(w, http.StatusCreated, EnrollmentResult{InstallationID: id})
}

// validManagement rejects fields unrelated to the signed mutation's action.
func validManagement(value ManagementPayload) bool {
	switch value.Action {
	case "rotate_token":
		return validDeviceToken(value.DeviceToken) &&
			value.RouteID == "" &&
			value.SubscriptionID == "" && !value.EnableAlerts

	case "subscribe":
		return validIdentifier(value.RouteID, 128) &&
			value.DeviceToken == "" &&
			value.SubscriptionID == ""

	case "revoke_subscription":
		return validIdentifier(value.SubscriptionID, 64) &&
			value.DeviceToken == "" &&
			value.RouteID == "" && !value.EnableAlerts

	case "delete_installation":
		return value.SubscriptionID == "" && value.DeviceToken == "" &&
			value.RouteID == "" && !value.EnableAlerts

	default:
		return false
	}
}

// manage verifies saved key, counter and publisher policy before atomically
// consuming its challenge and applying the authorized mutation.
func (s *Server) manage(w http.ResponseWriter, r *http.Request) {
	var proof ProofRequest
	if err := decodeJSON(r.Body, &proof); err != nil ||
		proof.KeyID != "" || len(proof.Proof) == 0 {

		writeError(w, http.StatusBadRequest, "invalid_request")

		return
	}
	id := r.PathValue("installation")
	challenge, err := s.store.loadChallenge(
		r.Context(), proof.ChallengeID, s.now(),
	)
	if err != nil || challenge.Operation != OperationManage ||
		challenge.InstallationID != id {

		writeError(w, http.StatusUnauthorized, "invalid_proof")

		return
	}
	owner, err := s.store.loadInstallation(r.Context(), id)
	app, supported := s.apps[challenge.AppID]
	if err != nil || !supported || owner.Policy != app {
		writeError(w, http.StatusUnauthorized, "unauthorized")

		return
	}
	var action ManagementPayload
	if err := decodeJSON(
		bytes.NewReader(proof.Payload), &action,
	); err != nil ||
		!validManagement(action) {

		writeError(w, http.StatusBadRequest, "invalid_payload")

		return
	}
	counter, err := s.verifier.assert(
		app, owner.PublicKey, owner.Counter, challenge, proof.Payload,
		proof.Proof,
	)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_proof")

		return
	}
	action.DeviceToken = strings.ToLower(action.DeviceToken)
	result, err := s.store.applyManagement(
		r.Context(), challenge, counter, action, s.now(),
	)
	if err != nil {
		storeError(w, err)

		return
	}
	writeJSON(w, http.StatusOK, result)
}

// bearerCapability bounds the only producer credential accepted by the relay.
func bearerCapability(r *http.Request) (string, bool) {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") ||
		!validIdentifier(parts[1], 43) || len(parts[1]) != 43 {
		return "", false
	}

	return parts[1], true
}

// verifySubscription checks issued capabilities without scheduling delivery.
// Producers call it after owner proof verification, before storing a route.
func (s *Server) verifySubscription(w http.ResponseWriter, r *http.Request) {
	wake, ok := bearerCapability(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")

		return
	}
	var request SubscriptionVerificationRequest
	if err := decodeJSON(r.Body, &request); err != nil ||
		!validIdentifier(request.SubscriptionID, 64) {

		writeError(w, http.StatusBadRequest, "invalid_request")

		return
	}
	policy, expiry, err := s.store.verifySubscription(
		r.Context(), wake, request, s.now(),
	)
	if err != nil {
		storeError(w, err)

		return
	}
	if app, ok := s.apps[policy.ID]; !ok || app != policy {
		writeError(w, http.StatusUnauthorized, "unauthorized")

		return
	}
	writeJSON(w, http.StatusOK, SubscriptionVerificationResult{
		ExpiresAt: expiry,
	})
}

// push admits scoped jobs only. No destination, amount, title, topic or token
// can be supplied in the request body.
func (s *Server) push(scope Scope) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret, ok := bearerCapability(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")

			return
		}
		var request PushRequest
		if err := decodeJSON(r.Body, &request); err != nil ||
			!validIdentifier(request.EventID, 128) {

			writeError(w, http.StatusBadRequest, "invalid_request")

			return
		}
		if err := s.store.enqueue(
			r.Context(), secret, scope, request.EventID, s.now(),
		); err != nil {

			storeError(w, err)

			return
		}
		s.requests[scopeIndex(scope)].Add(1)
		select {
		case s.wake <- struct{}{}:
		default:
		}
		writeJSON(w, http.StatusAccepted, struct{}{})
	}
}
