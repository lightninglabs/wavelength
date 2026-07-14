package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBrowserHeadersRequiresAllowedOrigin(t *testing.T) {
	t.Parallel()

	var called bool
	handler := BrowserHeaders(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			called = true
		}),
		nil,
	)

	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", "https://wallet.example")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.False(t, called)
}

func TestBrowserHeadersAllowsTrustedOrigin(t *testing.T) {
	t.Parallel()

	handler := BrowserHeaders(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatalf("preflight must not reach wrapped handler")
		}),
		[]string{
			"https://wallet.example",
		}, "x-waved-auth",
	)

	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", "https://wallet.example")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(
		t, "https://wallet.example",
		rec.Header().Get("Access-Control-Allow-Origin"),
	)
	require.Contains(
		t, rec.Header().Get("Access-Control-Allow-Headers"),
		"x-waved-auth",
	)
}

func TestBrowserHeadersAllowsWildcardOrigin(t *testing.T) {
	t.Parallel()

	handler := BrowserHeaders(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatalf("preflight must not reach wrapped handler")
		}),
		[]string{
			"*",
		}, "x-waved-auth",
	)

	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", "https://any-wallet.example")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(
		t, "*", rec.Header().Get("Access-Control-Allow-Origin"),
	)
	require.Contains(
		t, rec.Header().Get("Access-Control-Allow-Headers"),
		"x-waved-auth",
	)
}

func TestBrowserHeadersPassesNonBrowserRequests(t *testing.T) {
	t.Parallel()

	var called bool
	handler := BrowserHeaders(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			called = true
		}),
		nil,
	)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	require.True(t, called)
}
