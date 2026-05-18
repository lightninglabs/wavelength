package swaps

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/darepo-client/swaprpc"
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

				body, err := protojson.Marshal(
					&swaprpc.RequestChannelIdResponse{
						RouteHint: routeHint,
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
	hint, err := client.RequestChannelID(
		t.Context(), clientPriv.PubKey(), lntypes.Hash{1}, 30,
	)
	require.NoError(t, err)
	require.Equal(t, uint64(42), hint.ChannelID)
	require.Equal(t, nodeID, hint.NodeID)
}
