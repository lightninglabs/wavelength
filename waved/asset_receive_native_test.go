//go:build !js || !wasm

package waved

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/db"
	"github.com/lightningnetwork/lnd/cert"
	"github.com/stretchr/testify/require"
)

// TestAssetReceiveConfig builds the engine's receive configuration from the
// courier settings. A daemon without a courier still gets one, so it keeps
// receiving at the addresses it has, and a certificate that cannot be read
// stops the start.
func TestAssetReceiveConfig(t *testing.T) {
	t.Parallel()

	certPath := filepath.Join(t.TempDir(), "tls.cert")
	certBytes, _, err := cert.GenCertPair(
		"waved test", nil, nil, false, time.Hour,
	)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(certPath, certBytes, 0o600))

	server := &Server{cfg: DefaultConfig()}
	store := &db.AssetEngineStore{}
	keys := &assetKeyRing{server: server}

	receive, err := server.assetReceiveConfig(store, keys)
	require.NoError(t, err)
	require.Nil(t, receive.DefaultCourier)
	require.NotNil(t, receive.Dial)
	require.Same(t, store, receive.Store)
	require.Same(t, keys, receive.Keys)

	server.cfg.Assets.Courier = AssetCourierConfig{
		Addr: testCourierAddr,
		Endpoints: []string{
			"tapd:10029=127.0.0.1:12029=" + certPath,
		},
	}
	receive, err = server.assetReceiveConfig(store, keys)
	require.NoError(t, err)
	require.Equal(t, testCourierAddr, receive.DefaultCourier.String())

	server.cfg.Assets.Courier.Endpoints = []string{
		"tapd:10029=127.0.0.1:12029=" + certPath + ".missing",
	}
	_, err = server.assetReceiveConfig(store, keys)
	require.ErrorContains(t, err, "read TLS certificate")

	notPEM := filepath.Join(t.TempDir(), "not.pem")
	require.NoError(t, os.WriteFile(notPEM, []byte("not a cert"), 0o600))
	_, err = courierTLSConfig(notPEM)
	require.ErrorContains(t, err, "holds no PEM certificate")
}
