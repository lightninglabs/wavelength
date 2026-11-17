package waved

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/rpc/restclient"
	"github.com/lightninglabs/wavelength/serverconn/mailboxpull"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// testOperatorBackoff keeps the retry loop's sleeps negligible in tests.
var testOperatorBackoff = mailboxpull.BackoffConfig{
	BaseDelay: time.Millisecond,
	MaxDelay:  time.Millisecond,
}

// TestRetryWhileOperatorUnreachable pins which failures the startup connect
// retries: an unreachable operator until it answers, nothing that retrying
// cannot fix, and nothing once the daemon shuts down.
func TestRetryWhileOperatorUnreachable(t *testing.T) {
	t.Parallel()

	// The negotiation wraps the transport error, so the code has to be
	// found through the wrapping.
	unreachable := fmt.Errorf("negotiate ark protocol version: %w",
		status.Error(codes.Unavailable, "connection refused"))
	incompatible := errors.New("no compatible ark protocol version")

	tests := []struct {
		name string

		// failures lists the errors of the attempts before the one
		// that succeeds.
		failures []error

		wantErr      error
		wantAttempts int
		wantRetries  int
	}{{
		name:         "operator answers at once",
		wantAttempts: 1,
	}, {
		name: "operator comes back",
		failures: []error{
			unreachable, unreachable,
			status.Error(codes.DeadlineExceeded, "timeout"),
		},
		wantAttempts: 4,
		wantRetries:  3,
	}, {
		name: "incompatible operator",
		failures: []error{
			incompatible,
		},
		wantErr:      incompatible,
		wantAttempts: 1,
	}, {
		name: "incompatible after unreachable",
		failures: []error{
			unreachable,
			incompatible,
		},
		wantErr:      incompatible,
		wantAttempts: 2,
		wantRetries:  1,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var attempts, retries int
			attempt := func(context.Context) (string, error) {
				attempts++
				if attempts <= len(tc.failures) {
					return "", tc.failures[attempts-1]
				}

				return "negotiated", nil
			}
			onRetry := func(attempt int, _ error) {
				retries++
				require.Equal(t, retries, attempt)
			}

			got, err := retryWhileOperatorUnreachable(
				t.Context(), testOperatorBackoff, attempt,
				onRetry,
			)
			require.Equal(t, tc.wantAttempts, attempts)
			require.Equal(t, tc.wantRetries, retries)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				require.Empty(t, got)

				return
			}
			require.NoError(t, err)
			require.Equal(t, "negotiated", got)
		})
	}
}

// TestRetryWhileOperatorUnreachableStopsOnShutdown pins that a daemon shutting
// down while the operator is still unreachable stops retrying and reports the
// shutdown, so the wallet-services signal resolves with it instead of never.
func TestRetryWhileOperatorUnreachableStopsOnShutdown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var attempts int
	attempt := func(context.Context) (string, error) {
		attempts++
		if attempts == 3 {
			cancel()
		}

		return "", status.Error(codes.Unavailable, "connection refused")
	}

	_, err := retryWhileOperatorUnreachable(
		ctx, testOperatorBackoff, attempt,
		func(int, error) {},
	)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "connection refused")
	require.Equal(t, 3, attempts)
}

// closedLoopbackAddr returns a loopback address that nothing listens on, so a
// dial to it is refused the way a dial to a stopped operator is.
func closedLoopbackAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	return addr
}

// restGetInfoErr returns the error a REST GetInfo against baseURL fails with.
func restGetInfoErr(t *testing.T, baseURL string) error {
	t.Helper()

	err := restclient.New(baseURL).Post(
		t.Context(), "/v1/ark/get-info", &arkrpc.GetInfoRequest{},
		&arkrpc.GetInfoResponse{},
	)
	require.Error(t, err)

	return err
}

// TestIsOperatorUnreachableREST pins which REST transport failures the
// startup connect retries. The REST client returns network errors without a
// gRPC code, so they are classified from the error itself: an operator that
// does not answer is retried, a TLS or request failure is not.
func TestIsOperatorUnreachableREST(t *testing.T) {
	t.Parallel()

	statusServer := func(code int, body string) string {
		server := httptest.NewServer(
			http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(code)
					_, _ = w.Write([]byte(body))
				},
			),
		)
		t.Cleanup(server.Close)

		return server.URL
	}

	tests := []struct {
		name string
		err  func(t *testing.T) error
		want bool
	}{{
		name: "connection refused",
		err: func(t *testing.T) error {
			return restGetInfoErr(
				t, "http://"+closedLoopbackAddr(t),
			)
		},
		want: true,
	}, {
		name: "connection reset",
		err: func(*testing.T) error {
			return fmt.Errorf("GetInfo RPC: %w", &net.OpError{
				Op:  "read",
				Net: "tcp",
				Err: os.NewSyscallError(
					"read", syscall.ECONNRESET,
				),
			})
		},
		want: true,
	}, {
		name: "gateway reports unavailable",
		err: func(t *testing.T) error {
			return restGetInfoErr(
				t, statusServer(
					http.StatusServiceUnavailable, "down",
				),
			)
		},
		want: true,
	}, {
		name: "untrusted TLS certificate",
		err: func(t *testing.T) error {
			server := httptest.NewTLSServer(http.NotFoundHandler())
			t.Cleanup(server.Close)

			return restGetInfoErr(t, server.URL)
		},
	}, {
		name: "HTTPS to a plaintext operator",
		err: func(t *testing.T) error {
			server := httptest.NewServer(http.NotFoundHandler())
			t.Cleanup(server.Close)

			return restGetInfoErr(
				t, strings.Replace(
					server.URL, "http://", "https://", 1,
				),
			)
		},
	}, {
		name: "gateway rejects request",
		err: func(t *testing.T) error {
			return restGetInfoErr(
				t, statusServer(
					http.StatusBadRequest, "bad request",
				),
			)
		},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.err(t)
			require.Equal(
				t, tc.want, isOperatorUnreachable(err),
				"error: %v", err,
			)
		})
	}
}

// TestConnectAndNegotiateRetriesRefusedREST pins that a daemon on the REST
// transport keeps retrying an operator that refuses connections and finishes
// the negotiation once the operator listens again, without a restart.
func TestConnectAndNegotiateRetriesRefusedREST(t *testing.T) {
	t.Parallel()

	addr := closedLoopbackAddr(t)
	s := &Server{
		cfg: &Config{
			Server: &ServerConfig{
				Host:      "http://" + addr,
				Transport: RPCTransportREST,
				Insecure:  true,
			},
		},
		log: btclog.Disabled,
	}

	type result struct {
		negotiation *arkVersionNegotiation
		err         error
	}
	done := make(chan result, 1)
	go func() {
		negotiation, err := s.connectAndNegotiateWithRetry(t.Context())
		done <- result{negotiation: negotiation, err: err}
	}()

	// The first attempt is refused at once. A loop that gave up on it
	// would report that refusal well within this window.
	select {
	case res := <-done:
		t.Fatalf("connect gave up on a refused operator: %v", res.err)

	case <-time.After(500 * time.Millisecond):
	}

	// Bring the operator up on the address the daemon keeps dialing.
	getInfo, err := protojson.Marshal(&arkrpc.GetInfoResponse{
		Pubkey:             testOperatorPubKeyBytes(t),
		SelectedArkVersion: arkrpc.ArkProtocolVersionV1,
	})
	require.NoError(t, err)

	listener, err := net.Listen("tcp", addr)
	require.NoError(t, err)

	operator := httptest.NewUnstartedServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(
					"Content-Type", "application/json",
				)
				_, _ = w.Write(getInfo)
			},
		),
	)
	require.NoError(t, operator.Listener.Close())
	operator.Listener = listener
	operator.Start()
	defer operator.Close()

	select {
	case res := <-done:
		require.NoError(t, res.err)
		require.Equal(
			t, arkrpc.ArkProtocolVersionV1,
			res.negotiation.selectedArkVersion,
		)

	case <-time.After(10 * time.Second):
		t.Fatal("connect did not finish after the operator came back")
	}
}
