package main

import (
	"context"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/notifyrelay"
	"github.com/lightninglabs/wavelength/notifyrelay/relaydb"
	"github.com/stretchr/testify/require"
)

// unusedProvider proves an empty relay never performs provider I/O.
type unusedProvider struct{}

// ValidateApplication retains the reference provider's startup policy.
func (unusedProvider) ValidateApplication(app notifyrelay.AppConfig) error {
	return (&notifyrelay.APNSProvider{}).ValidateApplication(app)
}

// NormalizeDeviceToken retains APNs token policy without provider I/O.
func (unusedProvider) NormalizeDeviceToken(token string) (string, error) {
	return (&notifyrelay.APNSProvider{}).NormalizeDeviceToken(token)
}

// Send rejects unexpected delivery in the shutdown ownership test.
func (unusedProvider) Send(context.Context, notifyrelay.Delivery) (
	notifyrelay.ProviderResult, error) {

	panic("unexpected provider request")
}

// TestServeShutdown proves successful shutdown joins the worker and releases
// database pool before returning to the caller.
func TestServeShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	database, err := relaydb.OpenSQLite(t.Context(), path)
	require.NoError(t, err)
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	store, err := notifyrelay.NewSQLStore(
		t.Context(), database, notifyrelay.SourceConfig{
			ID:       "producer",
			Role:     "operator",
			Endpoint: "127.0.0.1:1",
			PublicKey: hex.EncodeToString(
				key.PubKey().SerializeCompressed(),
			),
			EventClasses: 1,
		},
	)
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
		done <- serve(ctx, relay, database, listener)
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
	require.ErrorContains(
		t,
		database.DB.PingContext(
			t.Context(),
		),
		"database is closed",
	)
	reopened, err := relaydb.OpenSQLite(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, reopened.Close())
}

// TestRunRequiresTLSIngress rejects an exposed cleartext listener before any
// credentials, database or server can be opened.
func TestRunRequiresTLSIngress(t *testing.T) {
	err := run(context.Background(), []string{"--listen=0.0.0.0:8080"})
	require.EqualError(t, err, "external HTTP requires --behind-tls-proxy")
}

// TestSQLDatabaseSelection never treats a legacy/non-SQL file as a relay
// database and never exposes mounted PostgreSQL credentials in an error.
func TestSQLDatabaseSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old-relay.db")
	original := []byte("not a SQLite database")
	require.NoError(t, os.WriteFile(path, original, 0600))
	_, err := openDatabase(t.Context(), path, "")
	require.Error(t, err)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, after)
	dsnPath := filepath.Join(t.TempDir(), "postgres-secret")
	require.NoError(
		t,
		os.WriteFile(
			dsnPath, []byte(
				"postgres://secret-user:private-password@inv"+
					"alid:1/relay",
			),
			0600,
		),
	)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err = openDatabase(
		ctx,
		filepath.Join(
			t.TempDir(),
			"unused.db",
		),
		dsnPath,
	)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "private-password")
	require.NotContains(t, err.Error(), "secret-user")
}

// TestPublisherPolicyJSON rejects silently ignored fields and trailing policy.
func TestPublisherPolicyJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sources.json")
	var sources []notifyrelay.SourceConfig
	require.NoError(
		t,
		os.WriteFile(
			path, []byte(`[{"id":"producer","unrecognized":true}]`),
			0600,
		),
	)
	require.Error(t, readPolicy(path, &sources))
	require.NoError(t, os.WriteFile(path, []byte(`[] []`), 0600))
	require.Error(t, readPolicy(path, &sources))
	require.NoError(t, os.WriteFile(path, []byte(`[]`), 0600))
	require.NoError(t, readPolicy(path, &sources))
	require.Empty(t, sources)
}
