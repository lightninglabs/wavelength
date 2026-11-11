package notifyrelay

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

// newServer injects deterministic dependencies for package tests only.
func newServer(cfg Config, store *Store, provider Provider,
	verifier installationVerifier, now func() time.Time) (*Server, error) {

	if verifier == nil {
		return nil, errors.New("relay verifier required")
	}

	server, err := newAuthenticatedServer(
		cfg, store, provider, &AppAttestAuthenticator{
			verifier: verifier,
		},
		now,
	)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle(
		"POST /v1/wakes",
		server.limitRequests(
			server.push(ScopeWake),
		),
	)
	mux.Handle(
		"POST /v1/alerts",
		server.limitRequests(
			server.push(ScopeAlert),
		),
	)
	mux.Handle(
		"POST /v1/subscriptions/verify",
		server.limitRequests(
			http.HandlerFunc(server.verifySubscription),
		),
	)
	mux.Handle("/", server.handler)
	server.handler = mux

	return server, nil
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
	store, ok := s.store.(*Store)
	if !ok {
		writeError(w, http.StatusNotFound, "retired")

		return
	}

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
	policy, expiry, err := store.verifySubscription(
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
		store, ok := s.store.(*Store)
		if !ok {
			writeError(w, http.StatusNotFound, "retired")

			return
		}
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
		if err := store.enqueue(
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
