package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

const metaHeader = "x-darepod-auth"

// TestBrowserHeaders exercises the CORS middleware across browser preflight,
// trusted/wildcard origins, non-browser passthrough, and allowed-origin
// forwarding of real (non-OPTIONS) requests.
func TestBrowserHeaders(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		origins       []string
		method        string
		origin        string
		wantCalled    bool
		wantCode      int
		wantAllowOrig string
	}{{
		name:     "rejects unlisted origin",
		method:   http.MethodOptions,
		origin:   "https://wallet.example",
		wantCode: http.StatusForbidden,
	}, {
		name: "allows trusted origin",
		origins: []string{
			"https://wallet.example",
		},
		method:        http.MethodOptions,
		origin:        "https://wallet.example",
		wantCode:      http.StatusNoContent,
		wantAllowOrig: "https://wallet.example",
	}, {
		name: "allows wildcard origin",
		origins: []string{
			"*",
		},
		method:        http.MethodOptions,
		origin:        "https://any-wallet.example",
		wantCode:      http.StatusNoContent,
		wantAllowOrig: "*",
	}, {
		name:       "passes non-browser requests",
		method:     http.MethodPost,
		wantCalled: true,
	}, {
		name: "forwards allowed non-options request",
		origins: []string{
			"https://wallet.example",
		},
		method:        http.MethodPost,
		origin:        "https://wallet.example",
		wantCalled:    true,
		wantAllowOrig: "https://wallet.example",
	}}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var called bool
			h := BrowserHeaders(
				http.HandlerFunc(func(http.ResponseWriter,
					*http.Request) {

					called = true
				}),
				tc.origins,
				metaHeader,
			)

			req := httptest.NewRequest(tc.method, "/", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			require.Equal(t, tc.wantCalled, called)
			if tc.wantCode != 0 {
				require.Equal(t, tc.wantCode, rec.Code)
			}
			if tc.wantAllowOrig == "" {
				return
			}
			hdr := rec.Header()
			require.Equal(
				t, tc.wantAllowOrig,
				hdr.Get("Access-Control-Allow-Origin"),
			)
			require.Contains(
				t, hdr.Get("Access-Control-Allow-Headers"),
				metaHeader,
			)
		})
	}
}
