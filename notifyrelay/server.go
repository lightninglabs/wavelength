package notifyrelay

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// appAttestRoot is Apple's dedicated App Attestation root, obtained from
// https://www.apple.com/certificateauthority/Apple_App_Attestation_Root_CA.pem.
//
//go:embed assets/Apple_App_Attestation_Root_CA.pem
var appAttestRoot []byte

// Config holds publisher-controlled relay policy. Applications are copied at
// construction. Logger receives metadata only, never proof bodies or tokens.
type Config struct {
	Applications []AppConfig
	// Authenticator defaults to App Attest; custom modules are trusted
	// publisher code selected at construction, never by HTTP clients.
	Authenticator DeviceAuthenticator
	Logger        *slog.Logger
}

// Server owns bounded HTTP admission and a durable delivery worker. The caller
// owns the SQL database and must join HTTP shutdown and Run before closing it.
type Server struct {
	store          managementStore
	provider       Provider
	verifier       DeviceAuthenticator
	apps           map[string]AppConfig
	log            *slog.Logger
	now            func() time.Time
	handler        http.Handler
	wake           chan struct{}
	slots          chan struct{}
	running        atomic.Bool
	sourceFailures atomic.Uint64
	requests       [2]atomic.Uint64
	delivered      [2][4]atomic.Uint64
}

// NewServer constructs a SQL subscriber relay. App Attest is the default
// module; Config.Authenticator can select an explicit publisher integration in
// Go. There is no HTTP or command-line attestation bypass.
func NewServer(cfg Config, store *SQLStore,
	provider Provider) (*Server, error) {

	if store == nil || len(store.sources) == 0 {
		return nil, errors.New("SQL relay sources required")
	}
	authenticator := cfg.Authenticator
	if authenticator == nil {
		var err error
		authenticator, err = NewAppAttestAuthenticator()
		if err != nil {
			return nil, err
		}
	}

	return newAuthenticatedServer(
		cfg, store, provider, authenticator, time.Now,
	)
}

// newAuthenticatedServer shares HTTP/store ownership across publisher modules.
func newAuthenticatedServer(cfg Config, store managementStore,
	provider Provider, verifier DeviceAuthenticator,
	now func() time.Time) (*Server, error) {

	if store == nil || provider == nil || verifier == nil ||
		now == nil || len(cfg.Applications) == 0 {
		return nil, errors.New("relay dependencies and apps required")
	}
	apps := make(map[string]AppConfig, len(cfg.Applications))
	for _, app := range cfg.Applications {
		if !validIdentifier(app.ID, 100) {
			return nil, errors.New("invalid relay app alias")
		}
		if err := verifier.ValidateApplication(app); err != nil {
			return nil, err
		}
		if err := provider.ValidateApplication(app); err != nil {
			return nil, err
		}
		if _, duplicate := apps[app.ID]; duplicate {
			return nil, errors.New("duplicate relay app alias")
		}
		apps[app.ID] = app
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &Server{
		store: store, provider: provider,
		verifier: verifier, apps: apps,
		log: logger, now: now, wake: make(chan struct{}, 1),
		slots: make(chan struct{}, 64),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/challenges", s.challenge)
	mux.HandleFunc("POST /v1/installations", s.enroll)
	mux.HandleFunc("POST /v1/installations/{installation}/manage", s.manage)
	mux.HandleFunc(
		"GET /healthz",
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	)
	mux.HandleFunc(
		"GET /readyz",
		func(w http.ResponseWriter, _ *http.Request) {
			if !s.running.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)

				return
			}
			w.WriteHeader(http.StatusOK)
		},
	)
	mux.HandleFunc("GET /metrics", s.metrics)
	s.handler = s.limitRequests(mux)

	return s, nil
}

// limitRequests bounds concurrent admission and untrusted JSON before routing.
func (s *Server) limitRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			select {
			case s.slots <- struct{}{}:
				defer func() {
					<-s.slots
				}()

			default:
				writeError(
					w, http.StatusTooManyRequests, "busy",
				)

				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
			next.ServeHTTP(w, r)
		},
	)

}

// Handler exposes the relay API behind request deadlines and TLS termination.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// decodeJSON rejects unknown fields and trailing JSON values.
func decodeJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}

	return nil
}

// writeJSON emits explicitly constructed public responses without caching.
func writeJSON(w http.ResponseWriter, status int, response any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(response); err != nil {

		// The response may already be partly written. Do not append an
		// error body or log the private response payload.
		return
	}
}

// writeError keeps verifier diagnostics and private records out of responses.
func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{Error: code})
}

// storeError maps expected admission failures to public error codes.
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrCapacity):
		writeError(w, http.StatusTooManyRequests, "capacity")

	case errors.Is(err, ErrReplay):
		writeError(w, http.StatusConflict, "replayed_proof")

	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusUnauthorized, "unauthorized")

	default:
		writeError(w, http.StatusServiceUnavailable, "unavailable")
	}
}

// scopeIndex gives metrics a fixed two-value label space.
func scopeIndex(scope Scope) int {
	if scope == ScopeAlert {
		return 1
	}

	return 0
}

// DeliverOnce processes at most one job without idle waiting. Its provider
// timeout is shorter than the durable claim, and completion is claim-fenced.
func (s *Server) DeliverOnce(ctx context.Context) (bool, error) {
	delivery, err := s.store.claim(ctx, s.now())
	if err != nil || delivery == nil {
		return false, err
	}
	app, supported := s.apps[delivery.policy.ID]
	result := ProviderResult{Outcome: OutcomeRejected}
	if supported && app == delivery.policy {
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		result, err = s.provider.Send(sendCtx, *delivery)
		cancel()
		if err != nil {
			// The provider classifies permanent failures separately
			// from retryable transport errors. Preserve that
			// decision.
			s.log.DebugContext(
				ctx, "Notification provider request failed",
				slog.String("scope", string(delivery.Scope)),
				slog.Any("error", err),
			)
		}
	}
	// Cancellation leaves the lease for the next owner. No detached store
	// operation can outlive the worker's successful return.
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if err := s.store.complete(
		ctx, *delivery, result, s.now(),
	); err != nil {
		return true, err
	}
	if result.Outcome <= OutcomeRejected {
		s.delivered[scopeIndex(
			delivery.Scope)][int(result.Outcome)].Add(1)
	}

	return true, nil
}

// Run owns the worker until cancellation. Returning joins its provider call
// and store work. Concurrent Run calls are rejected.
func (s *Server) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("notification worker already running")
	}
	defer s.running.Store(false)
	runCtx, cancel := context.WithCancel(ctx)
	var sources sync.WaitGroup
	if store, ok := s.store.(*SQLStore); ok {
		for id := range store.sources {
			sources.Go(
				func() {
					s.runFeedSource(runCtx, store, id)
				},
			)
		}
	}
	defer func() {
		cancel()
		sources.Wait()
	}()
	ctx = runCtx
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := s.store.pruneExpired(ctx, s.now()); err != nil {
			if ctx.Err() != nil {
				return nil
			}

			return err
		}
		for {
			worked, err := s.DeliverOnce(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}

				return err
			}
			if !worked {
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil

		case <-ticker.C:
		case <-s.wake:
		}
	}
}

// metrics exposes bounded labels only. It belongs on the internal metrics
// service, not the relay's public ingress paths.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	for _, scope := range []Scope{ScopeWake, ScopeAlert} {
		index := scopeIndex(scope)
		for outcome, label := range []string{
			"retry",
			"accepted",
			"invalid_token",
			"rejected",
		} {
			_, _ = fmt.Fprintf(
				w, "notification_relay_delivery_attempts_tot"+
					"al{scope=%q,outcome=%q} %d\n", scope,
				label, s.delivered[index][outcome].Load(),
			)
		}
	}
	_, _ = fmt.Fprintf(
		w, "notification_relay_source_failures_total %d\n",
		s.sourceFailures.Load(),
	)
	pending, err := s.store.pendingJobs(r.Context())
	if err == nil {
		_, _ = fmt.Fprintf(
			w, "notification_relay_pending_jobs %d\n", pending,
		)
	}
}
