# gateway

## Purpose

HTTP gateway utilities for grpc-gateway integration. Provides standard mux
options, CORS middleware, and endpoint normalization so REST-to-gRPC bridges
have a consistent configuration across all daemon sub-services.

## Key Types

- `ServeMuxOptions(headerMatcher) []runtime.ServeMuxOption` — Returns
  standard grpc-gateway configuration: JSON marshaling with
  `UseProtoNames=true` (snake_case) and `EmitUnpopulated=true`, path-length
  fallback disabled, and an optional header matcher.
- `BrowserHeaders(handler, allowedOrigins, metadataHeaders) http.Handler` —
  CORS middleware. Requests without an `Origin` header pass through
  unconditionally (non-browser clients are not restricted); requests
  carrying an `Origin` are validated against the allowlist and rejected
  with 403 when not present. Injects CORS headers and allows GET, POST,
  and OPTIONS methods.
- `NormalizeEndpoint(endpoint) string` — Converts listener addresses for
  loopback dialing: `0.0.0.0` → `127.0.0.1`, `[::]` → `[::1]`, others
  pass through unchanged.

## Relationships

- **Depends on**: `google.golang.org/grpc`, `github.com/grpc-ecosystem/grpc-gateway/v2`
  (no repo packages).
- **Depended on by**: `darepod` (HTTP gateway setup for all sub-services).

## Invariants

- Browser callers (those sending an `Origin` header) need an entry in
  `allowedOrigins` or the request is rejected with 403. The wildcard
  value `"*"` allows any origin; use it only for APIs with per-request
  authentication. An empty allowlist means no browser caller can reach
  the gateway; non-browser clients without an `Origin` header are
  unaffected.
- When the wildcard `"*"` is present, the response sets
  `Access-Control-Allow-Origin: *` without a `Vary` header. For
  specific origins, `Vary: Origin` is set so CDN caches serve
  per-origin responses correctly.
- JSON marshaling always uses `UseProtoNames` (snake_case field names) and
  `EmitUnpopulated` (include zero/default values) for API consistency with
  grpc-gateway clients.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
