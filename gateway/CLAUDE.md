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
  with 403 when not present, unless `allowedOrigins` contains the
  wildcard `"*"`. Injects CORS headers and allows GET, POST, and OPTIONS
  methods.
- `NormalizeEndpoint(endpoint) string` — Converts listener addresses for
  loopback dialing: `0.0.0.0` → `127.0.0.1`, `[::]` → `[::1]`, others
  pass through unchanged.

## Relationships

- **Depends on**: `github.com/grpc-ecosystem/grpc-gateway/v2/runtime`,
  `google.golang.org/protobuf/encoding/protojson` (no repo packages).
- **Depended on by**: `darepod` (HTTP gateway setup for all sub-services).

## Invariants

- Browser callers (those sending an `Origin` header) need an entry in
  `allowedOrigins` or the request is rejected with 403, unless the
  allowlist contains `"*"` (allow-all, only fit for APIs with explicit
  per-request auth). An empty allowlist means no browser caller can
  reach the gateway; non-browser clients without an `Origin` header are
  unaffected.
- JSON marshaling always uses `UseProtoNames` (snake_case field names) and
  `EmitUnpopulated` (include zero/default values) for API consistency with
  grpc-gateway clients.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
