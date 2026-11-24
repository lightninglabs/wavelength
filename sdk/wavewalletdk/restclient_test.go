package wavewalletdk

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// walletRoundTrip lets transport tests inspect the HTTP request without
// binding a TCP listener (including in a JS VM).
type walletRoundTrip func(*http.Request) (*http.Response, error)

// RoundTrip serves the request using the test gateway.
func (rt walletRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	return rt(req)
}

// TestArkChannelReceiveREST checks both dispatch and fail-closed behavior
// against an older gateway that has no channel receive method.
func TestArkChannelReceiveREST(t *testing.T) {
	var paths []string
	var reject bool
	httpClient := &http.Client{Transport: walletRoundTrip(
		func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.URL.Path)
			code, body := http.StatusOK, "{}"
			switch req.URL.Path {
			case "/v1/daemon/get-info":
			case "/v1/wallet/recv-on-active-channel":
				raw, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.JSONEq(
					t, `{"amt_sat":"42","memo":"m"}`,
					string(raw),
				)
				body = `{"invoice":"ln-channel"}`
				if reject {
					code = http.StatusNotFound
					body = "not found"
				}

			default:
				t.Fatalf("unexpected fallback to %s",
					req.URL.Path)
			}

			return &http.Response{
				StatusCode: code, Body: io.NopCloser(
					strings.NewReader(body),
				),
			}, nil
		},
	)}
	client, err := Connect(t.Context(), ConnectConfig{
		Address: "https://wallet.invalid", Transport: TransportREST,
		HTTPClient: httpClient,
	})
	require.NoError(t, err)
	defer func() {
		require.NoError(t, client.Close())
	}()
	req := ReceiveRequest{AmountSat: 42, Memo: "m"}
	invoice, err := client.ReceiveOnActiveChannel(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "ln-channel", invoice.Invoice)
	reject = true
	invoice, err = client.ReceiveOnActiveChannel(t.Context(), req)
	require.Error(t, err)
	require.Nil(t, invoice)
	require.Len(t, paths, 3)
}

// TestConnectREST verifies wavewalletdk can use grpc-gateway clients without a
// native gRPC connection.
func TestConnectREST(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/v1/daemon/get-info", r.URL.Path)

			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(`{
				"network": "regtest",
				"wallet_state": "WALLET_STATE_READY",
				"server_info": {
					"free_refresh_window_blocks": 120
				}
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
	require.NotNil(t, info.ServerInfo)
	require.Equal(
		t, uint32(120), info.ServerInfo.FreeRefreshWindowBlocks,
	)
}
