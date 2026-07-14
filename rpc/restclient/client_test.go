package restclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestDaemonClientPost verifies that the generated-interface-shaped REST
// client posts protoJSON to the expected gateway route.
func TestDaemonClientPost(t *testing.T) {
	var sawMetadata bool
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/v1/daemon/get-info", r.URL.Path)
			require.Equal(
				t, "application/json", r.Header.Get(
					"Content-Type",
				),
			)
			require.Equal(
				t, "metadata-value", r.Header.Get(
					"X-Test-Metadata",
				),
			)
			require.Empty(
				t, r.Header.Values(
					"Grpc-Metadata-X-Test-Metadata",
				),
			)
			sawMetadata = true

			_, err := io.Copy(io.Discard, r.Body)
			require.NoError(t, err)

			w.Header().Set("Content-Type", "application/json")
			_, err = w.Write([]byte(`{"network":"regtest"}`))
			require.NoError(t, err)
		},
	))
	defer server.Close()

	client := NewDaemonServiceClient(server.URL)
	ctx := metadata.AppendToOutgoingContext(
		t.Context(),
		"x-test-metadata", "metadata-value",
	)
	resp, err := client.GetInfo(ctx, &waverpc.GetInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "regtest", resp.GetNetwork())
	require.True(t, sawMetadata)
}

// TestDaemonClientGetVTXOExpiryInfoPost verifies the VTXO expiry RPC uses the
// grpc-gateway route exposed by daemon.yaml.
func TestDaemonClientGetVTXOExpiryInfoPost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			require.Equal(
				t, "/v1/daemon/get-vtxo-expiry-info",
				r.URL.Path,
			)

			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(
				`{"found":true,` +
					`"expiryInfo":{"status":` +
					`"VTXO_EXPIRY_STATUS_SAFE"}}`,
			))
			require.NoError(t, err)
		},
	))
	defer server.Close()

	client := NewDaemonServiceClient(server.URL)
	resp, err := client.GetVTXOExpiryInfo(
		t.Context(), &waverpc.GetVTXOExpiryInfoRequest{},
	)
	require.NoError(t, err)
	require.True(t, resp.GetFound())
	require.Equal(
		t, waverpc.VTXOExpiryStatus_VTXO_EXPIRY_STATUS_SAFE,
		resp.GetExpiryInfo().GetStatus(),
	)
}

func TestNewDefaultsSchemelessRemoteToHTTPS(t *testing.T) {
	t.Parallel()

	client := New("daemon.example.com:1234")
	require.Equal(t, "https://daemon.example.com:1234", client.baseURL)
}

func TestNewDefaultsSchemelessLoopbackToHTTP(t *testing.T) {
	t.Parallel()

	cases := []string{
		"localhost:10031",
		"127.0.0.1:10031",
		"[::1]:10031",
	}
	for _, addr := range cases {
		t.Run(addr, func(t *testing.T) {
			client := New(addr)
			require.True(
				t, strings.HasPrefix(client.baseURL, "http://"),
				client.baseURL,
			)
		})
	}
}

// TestSwapServiceClientPost verifies the external swap server REST client uses
// the swaprpc gateway routes.
func TestSwapServiceClientPost(t *testing.T) {
	settlementType := swaprpc.SettlementType_SETTLEMENT_TYPE_LIGHTNING
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				require.Equal(
					t, "/v1/swap/create-in-swap",
					r.URL.Path,
				)

				body, err := protojson.Marshal(
					&swaprpc.CreateInSwapResponse{
						PaymentHash:  make([]byte, 32),
						AmountSat:    1000,
						FeeSat:       1,
						ServerPubkey: []byte{2},
						Expiry: timestamppb.New(
							time.Date(
								2026, 5, 16, 0,
								0, 0, 0,
								time.UTC,
							),
						),
						SettlementType: settlementType,
					},
				)
				require.NoError(t, err)

				w.Header().Set(
					"Content-Type", "application/json",
				)
				_, err = w.Write(body)
				require.NoError(t, err)
			},
		),
	)
	defer server.Close()

	client := NewSwapServiceClient(server.URL)
	resp, err := client.CreateInSwap(
		t.Context(), &swaprpc.CreateInSwapRequest{},
	)
	require.NoError(t, err)
	require.Equal(t, uint64(1000), resp.GetAmountSat())
	require.Equal(
		t, swaprpc.SettlementType_SETTLEMENT_TYPE_LIGHTNING,
		resp.GetSettlementType(),
	)
}

// TestGatewayStatusError verifies grpc-gateway error envelopes are converted
// back into gRPC status errors.
func TestGatewayStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, err := w.Write([]byte(
				`{"code":"5","message":"missing"}`,
			))
			require.NoError(t, err)
		},
	))
	defer server.Close()

	client := NewDaemonServiceClient(server.URL)
	_, err := client.GetInfo(t.Context(), &waverpc.GetInfoRequest{})
	require.Error(t, err)
	require.Equal(t, codes.NotFound, status.Code(err))
	require.ErrorContains(t, err, "missing")
}

// TestWalletStream verifies grpc-gateway stream chunks are decoded into the
// generated server-streaming client shape.
func TestWalletStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/v1/wallet/subscribe", r.URL.Path)

			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(
				`{"result":{"cursor":"5",` +
					`"entry":{"id":"entry-1"}}}` + "\n",
			))
			require.NoError(t, err)
		},
	))
	defer server.Close()

	client := NewWalletServiceClient(server.URL)
	stream, err := client.SubscribeWallet(
		t.Context(), &walletdkrpc.SubscribeWalletRequest{},
	)
	require.NoError(t, err)

	resp, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, int64(5), resp.GetCursor())
	require.Equal(t, "entry-1", resp.GetEntry().GetId())

	_, err = stream.Recv()
	require.True(t, errors.Is(err, io.EOF))
}

// TestWalletStreamError verifies stream error chunks become gRPC status
// errors.
func TestWalletStreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(
				`{"error":{"code":"14","message":"away"}}` +
					"\n",
			))
			require.NoError(t, err)
		},
	))
	defer server.Close()

	client := NewWalletServiceClient(server.URL)
	stream, err := client.SubscribeWallet(
		context.Background(), &walletdkrpc.SubscribeWalletRequest{},
	)
	require.NoError(t, err)

	_, err = stream.Recv()
	require.Error(t, err)
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.ErrorContains(t, err, "away")
}
