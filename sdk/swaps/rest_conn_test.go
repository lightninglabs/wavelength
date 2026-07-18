package swaps

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

// TestRESTSwapServerConnRequestChannelID verifies the REST swap server
// connection uses the swaprpc gateway routes through the shared REST client.
func TestRESTSwapServerConnRequestChannelID(t *testing.T) {
	nodePriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	nodeID := nodePriv.PubKey().SerializeCompressed()
	routeHint := &swaprpc.RouteHint{
		NodeId:             nodeID,
		ChannelId:          42,
		FeeBaseMsat:        1,
		FeeProportionalPpm: 2,
		CltvExpiryDelta:    40,
	}

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				require.Equal(
					t, "/v1/swap/request-channel-id",
					r.URL.Path,
				)

				routeHintPaths := []*swaprpc.RouteHintPath{{
					Hops: []*swaprpc.RouteHint{
						routeHint,
					},
				}}
				body, err := protojson.Marshal(
					&swaprpc.RequestChannelIdResponse{
						RouteHintPaths: routeHintPaths,
						PayerFeeMsat:   123,
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

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	client := NewRESTSwapServerConn(server.URL)
	quote, err := client.RequestChannelID(
		t.Context(), clientPriv.PubKey(), lntypes.Hash{1},
		btcutil.Amount(42_000), 30, true,
	)
	require.NoError(t, err)
	require.Len(t, quote.RouteHintPaths, 1)
	require.Len(t, quote.RouteHintPaths[0], 1)
	hint := quote.RouteHintPaths[0][0]
	require.Equal(t, uint64(42), hint.ChannelID)
	require.Equal(t, nodeID, hint.NodeID)
	require.EqualValues(t, 42_000, quote.ReceiveAmountSat)
	require.EqualValues(t, 123, quote.PayerFeeMsat)
}
