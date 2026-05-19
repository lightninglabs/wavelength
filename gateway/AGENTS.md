# gateway

## Purpose

Shared HTTP/JSON gateway helpers for darepo's grpc-gateway subservers.
Centralizes CORS header handling, endpoint normalization, and grpc-gateway
`ServeMux` option construction so each daemon subserver uses identical
transport configuration.

## Key Types

- `BrowserHeaders` — HTTP middleware that injects CORS response headers for
  grpc-gateway endpoints. Fails closed (no CORS headers) when
  `allowedOrigins` is empty, while still serving requests that lack an
  `Origin` header.
- `NormalizeEndpoint` — Returns a loopback-dialable address string for a
  gateway reverse-proxy dial (normalizes host-only forms to `localhost:port`).
- `ServeMuxOptions` — Returns the standard `[]runtime.ServeMuxOption` slice
  used by all darepo HTTP gateways, including the header matcher for gRPC
  metadata passthrough.

## Relationships

- **Depends on**: `github.com/grpc-ecosystem/grpc-gateway/v2/runtime` (ServeMux
  options and header matcher)
- **Depended on by**: `darepod` (gateway_server.go wires the daemon HTTP
  gateway using these helpers)

## Invariants

- This package is intentionally thin: it only provides shared configuration
  primitives. Gateway registration (mounting proto handlers) happens at the
  call site in each subserver.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
