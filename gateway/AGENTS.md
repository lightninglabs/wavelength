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
  CORS middleware. Validates the `Origin` header against the allowlist;
  an empty allowlist closes access for browser callers (fail-closed).
  Injects CORS headers and allows GET, POST, and OPTIONS methods.
- `NormalizeEndpoint(endpoint) string` — Converts listener addresses for
  loopback dialing: `0.0.0.0` → `127.0.0.1`, `[::]` → `[::1]`, others
  pass through unchanged.

## Relationships

- **Depends on**: `google.golang.org/grpc`, `github.com/grpc-ecosystem/grpc-gateway/v2`
  (no repo packages).
- **Depended on by**: `darepod` (HTTP gateway setup for all sub-services).

## Invariants

- An empty `allowedOrigins` list fails closed — no browser can reach the
  gateway. This is intentional for production hardening; pass a non-empty
  list only when the deployment serves browser clients.
- JSON marshaling always uses `UseProtoNames` (snake_case field names) and
  `EmitUnpopulated` (include zero/default values) for API consistency with
  grpc-gateway clients.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
