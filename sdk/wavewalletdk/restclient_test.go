package wavewalletdk

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConnectREST verifies wavewalletdk can use grpc-gateway clients without a
// native gRPC connection.
func TestConnectREST(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/v1/daemon/get-info", r.URL.Path)

			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(`{
				"network": "regtest",
				"wallet_state": "WALLET_STATE_READY"
			}`))
			require.NoError(t, err)
		},
	))
	defer server.Close()

	client, err := Connect(t.Context(), ConnectConfig{
		Address:   server.URL,
		Transport: TransportREST,
	})
	require.NoError(t, err)
	defer func() {
		require.NoError(t, client.Close())
	}()

	require.Nil(t, client.GRPCConn())

	info, err := client.GetInfo(t.Context())
	require.NoError(t, err)
	require.Equal(t, "regtest", info.Network)
	require.Equal(t, WalletStateReady, info.WalletState)
}
