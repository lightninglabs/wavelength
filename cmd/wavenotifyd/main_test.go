package main

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/notifyrelay"
	"github.com/stretchr/testify/require"
)

// unusedProvider proves an empty relay never performs provider I/O.
type unusedProvider struct{}

// Send rejects unexpected delivery in the shutdown ownership test.
func (unusedProvider) Send(context.Context, notifyrelay.Delivery) (
	notifyrelay.ProviderResult, error) {

	panic("unexpected provider request")
}

// TestServeShutdown proves successful shutdown joins the worker and releases
// the database's exclusive process lock before returning to the caller.
func TestServeShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	store, err := notifyrelay.OpenStore(path)
	require.NoError(t, err)
	relay, err := notifyrelay.NewServer(notifyrelay.Config{
		Applications: []notifyrelay.AppConfig{{
			ID: "test", AppleAppID: "TESTTEAM01.com.example.wallet",
			Topic:             "com.example.wallet",
			AttestEnvironment: "production",
			APNSEnvironment:   "production",
		}},
	}, store, unusedProvider{})
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, relay, store, listener)
	}()
	client := &http.Client{Timeout: time.Second}
	require.Eventually(t, func() bool {
		response, err := client.Get(
			"http://" + listener.Addr().String() + "/readyz",
		)
		if err != nil {
			return false
		}
		_ = response.Body.Close()

		return response.StatusCode == http.StatusOK
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)

	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not join")
	}
	reopened, err := notifyrelay.OpenStore(path)
	require.NoError(t, err)
	require.NoError(t, reopened.Close())
}

// TestRunRequiresTLSIngress rejects an exposed cleartext listener before any
// credentials, database or server can be opened.
func TestRunRequiresTLSIngress(t *testing.T) {
	err := run(context.Background(), []string{"--listen=0.0.0.0:8080"})
	require.EqualError(t, err, "external HTTP requires --behind-tls-proxy")
}
