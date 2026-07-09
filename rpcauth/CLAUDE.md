# rpcauth

## Purpose

Shared helpers for securing darepod's gRPC/REST surface and its clients:
TLS certificate provisioning plus macaroon-based per-RPC credentials. Used by
both the daemon (server-side TLS/macaroon setup) and its clients (darepocli,
the wallet SDK) to dial in with matching credentials.

## Key Types

- `MacaroonMetadataKey` — The gRPC metadata / HTTP header key
  (`"macaroon"`) that carries the hex-encoded macaroon; shared by server
  (gateway header allow-list) and clients (dial options, HTTP headers) so
  both sides agree on the wire key.
- `EnsureTLSCert(certPath, keyPath, organization)` — Idempotently loads an
  existing cert/key pair or self-signs a new one (via `lnd/cert`) if neither
  file exists yet; errors on a partial pair.
- `ServerTLSCredentials(certPath, keyPath)` / `ClientTLSCredentials(certPath)`
  — Build gRPC `credentials.TransportCredentials` for the server and client
  sides of a TLS-secured connection respectively.
- `HTTPClientForCert(certPath)` — Builds an `*http.Client` trusting the given
  cert (or system roots if empty), for REST/gateway clients.
- `DialOptionFromFile(path)` — Loads a macaroon from disk and returns a
  `grpc.DialOption` that attaches it as per-RPC credentials.
- `HexFromFile(path)` — Reads a macaroon file and hex-encodes it, for callers
  that need the raw header value instead of a gRPC dial option (e.g. HTTP/
  gateway clients).

## Relationships

- **Depends on**: `github.com/lightningnetwork/lnd/macaroons` (macaroon gRPC
  credentials), `github.com/lightningnetwork/lnd/cert` (self-signed cert
  generation), `google.golang.org/grpc`/`credentials`,
  `gopkg.in/macaroon.v2`.
- **Depended on by**: `darepod` (`rpc_security.go` server TLS setup,
  `gateway_server.go` REST gateway TLS/macaroon forwarding,
  `outbound_clients.go` and `server.go` outbound gRPC dialing),
  `cmd/darepocli/darepoclicommands` (CLI client dial options),
  `sdk/walletdk` (wallet SDK gRPC/HTTP client setup).

## Invariants

- `EnsureTLSCert` treats "cert exists but key missing" (or vice versa) as an
  error rather than silently regenerating, to avoid clobbering a cert whose
  key was lost or vice versa.
- Generated and loaded certs enforce `tls.VersionTLS12` as the minimum TLS
  version on both server and client credential paths.
- Cert/key files written by `EnsureTLSCert` are chmod'd `0o600` and their
  parent directories `0o700`; callers must not relax these permissions.
- `MacaroonMetadataKey` must stay identical across `DialOptionFromFile`,
  `HexFromFile` callers, and the gateway's header allow-list in
  `darepod/gateway_server.go` — a mismatch silently drops macaroon auth on
  one side of the client/server pair.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
