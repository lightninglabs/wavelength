package darepo

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lightninglabs/darepo-client/gateway"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/stretchr/testify/require"
)

// TestGatewayBrowserHeadersHandlePreflight verifies browser-hosted demos can
// preflight grpc-gateway requests before issuing protojson POSTs.
func TestGatewayBrowserHeadersHandlePreflight(t *testing.T) {
	t.Parallel()

	handler := gateway.BrowserHeaders(
		http.HandlerFunc(
			func(http.ResponseWriter, *http.Request) {
				t.Fatal(
					"preflight should not call next " +
						"handler",
				)
			},
		),
		[]string{
			"http://127.0.0.1:3000",
		},
		serverconn.AuthHeaderKey,
	)

	req := httptest.NewRequest(http.MethodOptions, "/v1/ark/info", nil)
	req.Header.Set("Origin", "http://127.0.0.1:3000")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(
		t, "http://127.0.0.1:3000",
		rec.Header().Get("Access-Control-Allow-Origin"),
	)
	require.Contains(
		t, rec.Header().Get("Access-Control-Allow-Headers"),
		serverconn.AuthHeaderKey,
	)
}
