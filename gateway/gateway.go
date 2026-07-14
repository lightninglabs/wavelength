package gateway

import (
	"net/http"
	"strings"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/protobuf/encoding/protojson"
)

// ServeMuxOptions returns the standard grpc-gateway mux options used by
// wavelength HTTP gateways.
func ServeMuxOptions(
	headerMatcher runtime.HeaderMatcherFunc) []runtime.ServeMuxOption {

	opts := []runtime.ServeMuxOption{
		runtime.WithMarshalerOption(
			runtime.MIMEWildcard, &runtime.JSONPb{
				MarshalOptions: protojson.MarshalOptions{
					UseProtoNames:   true,
					EmitUnpopulated: true,
				},
				UnmarshalOptions: protojson.UnmarshalOptions{
					DiscardUnknown: true,
				},
			},
		),
		runtime.WithDisablePathLengthFallback(),
	}
	if headerMatcher != nil {
		opts = append(
			[]runtime.ServeMuxOption{
				runtime.WithIncomingHeaderMatcher(
					headerMatcher,
				),
			},
			opts...,
		)
	}

	return opts
}

// BrowserHeaders wraps an HTTP handler with browser CORS handling. Empty
// allowed origins fail closed for browser callers while still serving requests
// without an Origin header. The wildcard origin "*" allows browser callers
// from any origin and is only suitable for APIs with explicit per-request
// authentication.
func BrowserHeaders(next http.Handler, allowedOrigins []string,
	metadataHeaders ...string) http.Handler {

	allowedHeaders := []string{
		"content-type",
		"authorization",
	}
	allowedHeaders = append(allowedHeaders, metadataHeaders...)
	origins := make(map[string]struct{}, len(allowedOrigins))
	allowAllOrigins := false
	for _, origin := range allowedOrigins {
		origin = strings.TrimSpace(origin)
		if origin == "*" {
			allowAllOrigins = true

			continue
		}

		origins[origin] = struct{}{}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)

			return
		}

		if _, ok := origins[origin]; !ok && !allowAllOrigins {
			http.Error(
				w, "origin not allowed", http.StatusForbidden,
			)

			return
		}

		if allowAllOrigins {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else {
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		w.Header().Set("Access-Control-Allow-Headers",
			strings.Join(allowedHeaders, ", "))
		w.Header().Set("Access-Control-Allow-Methods",
			"GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)

			return
		}

		next.ServeHTTP(w, r)
	})
}

// NormalizeEndpoint returns a loopback-dialable endpoint for a gateway proxy.
func NormalizeEndpoint(endpoint string) string {
	switch {
	case strings.Contains(endpoint, "0.0.0.0"):
		return strings.Replace(endpoint, "0.0.0.0", "127.0.0.1", 1)

	case strings.Contains(endpoint, "[::]"):
		return strings.Replace(endpoint, "[::]", "[::1]", 1)

	default:
		return endpoint
	}
}
