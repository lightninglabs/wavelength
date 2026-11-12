package notifyrelay

import (
	"bytes"
	"net/http"
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
	); err != nil {

		writeError(w, http.StatusBadRequest, "invalid_payload")

		return
	}
	token, err := s.provider.NormalizeDeviceToken(payload.DeviceToken)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_payload")

		return
	}

	id, err := InstallationID(challenge.AppID, proof.KeyID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_key")

		return
	}
	key, err := s.verifier.Attest(
		r.Context(), app, proof.Proof, proof.KeyID,
		ClientData(challenge, proof.Payload),
	)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_proof")

		return
	}
	now := s.now()
	err = s.store.enroll(r.Context(), challenge, installation{
		ID: id, Policy: app,
		PublicKey: key.PublicKey, Receipt: key.Receipt,
		DeviceToken:   token,
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
	if value.Action != "bind_feed" && value.FeedBinding != nil {
		return false
	}
	presentation := value.Presentation
	if presentation != "" && (!value.EnableAlerts ||
		(presentation != PresentationGeneric &&
			presentation != PresentationReceiptHint)) {
		return false
	}

	switch value.Action {
	case "bind_feed":
		return value.FeedBinding != nil &&
			validIdentifier(value.SubscriptionID, 64) &&
			value.DeviceToken == "" && value.RouteID == "" &&
			!value.EnableAlerts && value.Presentation == ""

	case "rotate_token":
		return value.DeviceToken != "" &&
			value.RouteID == "" &&
			value.SubscriptionID == "" && !value.EnableAlerts &&
			value.Presentation == ""

	case "subscribe":
		return validIdentifier(value.RouteID, 128) &&
			value.DeviceToken == "" &&
			value.SubscriptionID == ""

	case "revoke_subscription":
		return validIdentifier(value.SubscriptionID, 64) &&
			value.DeviceToken == "" &&
			value.RouteID == "" && !value.EnableAlerts &&
			value.Presentation == ""

	case "delete_installation":
		return value.SubscriptionID == "" && value.DeviceToken == "" &&
			value.RouteID == "" && !value.EnableAlerts &&
			value.Presentation == ""

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
	counter, err := s.verifier.Assert(
		r.Context(), app, owner.PublicKey, owner.Counter, challenge,
		proof.Payload, proof.Proof,
	)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_proof")

		return
	}
	if action.Action == "rotate_token" {
		token, err := s.provider.NormalizeDeviceToken(
			action.DeviceToken,
		)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_payload")

			return
		}
		action.DeviceToken = token
	}
	result, err := s.store.applyManagement(
		r.Context(), challenge, counter, action, s.now(),
	)
	if err != nil {
		storeError(w, err)

		return
	}
	writeJSON(w, http.StatusOK, result)
}
