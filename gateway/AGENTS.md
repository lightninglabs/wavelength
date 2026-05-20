# gateway

## Purpose

Shared HTTP/JSON grpc-gateway utilities used by the daemon's REST gateway
server. Provides CORS middleware, endpoint normalization, and standard
`grpc-gateway` mux options so each subserver (daemon, swap, wallet) uses
identical gateway configuration.

## Key Types

- `BrowserHeaders(next, allowedOrigins, metadataHeaders)` — CORS middleware
  wrapping an HTTP handler. Empty `allowedOrigins` fails closed for browser
  callers (`Origin` header present) while still serving requests without an
  `Origin` header (CLI, local service calls). Adds standard `*-bin` base64
  encoding and optional metadata headers to `Access-Control-Expose-Headers`.
- `NormalizeEndpoint(endpoint) string` — Returns a loopback-dialable address
  for grpc-gateway reverse-proxy dialing. Replaces `0.0.0.0` with `127.0.0.1`
  so the gateway can dial its own gRPC server when bound on all interfaces.
- `ServeMuxOptions(headerMatcher) []runtime.ServeMuxOption` — Returns the
  standard set of `grpc-gateway` mux options: JSON proto marshaller with
  default field emission, incoming header matching, and outgoing header
  passthrough.

## Relationships

- **Depends on**: `google.golang.org/grpc/status` (error formatting),
  `github.com/grpc-ecosystem/grpc-gateway/v2/runtime` (ServeMux options).
- **Depended on by**: `darepod` (gateway_server.go wires these helpers into
  the HTTP gateway server).

## Invariants

- `BrowserHeaders` only applies CORS headers when a request carries an
  `Origin` header; non-browser callers (curl, gRPC gateway dialing its own
  listener) are unaffected.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
