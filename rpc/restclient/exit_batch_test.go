package restclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

// TestExitBatchPost preserves the exact selection and partial outcomes over
// REST.
func TestExitBatchPost(t *testing.T) {
	t.Parallel()

	targets := []string{"a:0", "b:1"}
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/v1/wallet/exit-batch", r.URL.Path)
			raw, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			var request wavewalletrpc.ExitBatchRequest
			require.NoError(t, protojson.Unmarshal(raw, &request))
			require.Equal(t, targets, request.GetOutpoints())
			require.Equal(
				t, "destination", request.GetOnchainAddress(),
			)
			w.Header().Set("Content-Type", "application/json")
			_, err = w.Write([]byte(`{"results":[
                {"outpoint":"a:0","outcome":"EXIT_BATCH_ACCEPTED"},
                {"outpoint":"b:1","outcome":"EXIT_BATCH_FAILED",
                 "error":"missing"}
            ],"onchain_address":"destination"}`))
			require.NoError(t, err)
		},
	))
	defer server.Close()

	response, err := NewWalletServiceClient(server.URL).ExitBatch(
		t.Context(), &wavewalletrpc.ExitBatchRequest{
			Outpoints:      targets,
			OnchainAddress: "destination",
		},
	)
	require.NoError(t, err)
	require.Len(t, response.GetResults(), 2)
	require.Equal(
		t, wavewalletrpc.ExitBatchOutcome_EXIT_BATCH_ACCEPTED,
		response.Results[0].Outcome,
	)
	require.Equal(
		t, wavewalletrpc.ExitBatchOutcome_EXIT_BATCH_FAILED,
		response.Results[1].Outcome,
	)
	require.Equal(t, "missing", response.Results[1].Error)
}
