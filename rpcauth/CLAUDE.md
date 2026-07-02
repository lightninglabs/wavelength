# rpcauth

## Purpose

Low-level, transport-focused helpers for gRPC/HTTP client and server
authentication: loading/serializing macaroons and provisioning TLS
cert/key material and credentials. It does not define macaroon
permissions or entity/action scoping — that policy lives in
`darepod` (see `darepod/rpc_auth.go`, `darepod/rpc_security.go`), which
consumes this package's primitives to enforce it.

## Key Types

- `MacaroonMetadataKey` — the gRPC metadata / HTTP header key
  (`"macaroon"`) that carries the serialized macaroon on the wire;
  used consistently by both server-side extraction (`darepod`) and
  client-side attachment (`darepod`, `sdk/walletdk`, `darepocli`).
- `DialOptionFromFile(path)` — loads a macaroon from disk and returns
  a `grpc.DialOption` that attaches it as per-RPC credentials.
- `HexFromFile(path)` — reads and hex-encodes a macaroon for use as a
  raw metadata/header value (e.g. REST gateway, outbound client HTTP
  headers).
- `EnsureTLSCert(certPath, keyPath, organization)` — idempotently
  loads an existing TLS keypair or generates+persists a new
  self-signed pair (via `lnd/cert`) with `0o600` file permissions.
- `ServerTLSCredentials` / `ClientTLSCredentials` — build
  `credentials.TransportCredentials` for gRPC server/client, pinned
  to TLS 1.2+.
- `HTTPClientForCert(certPath)` — builds an `*http.Client` whose root
  CA pool is pinned to the given cert (falls back to system roots
  when `certPath` is empty).

## Relationships

- **Depends on**: `github.com/lightningnetwork/lnd/macaroons` (macaroon
  gRPC credential wrapping), `github.com/lightningnetwork/lnd/cert`
  (self-signed cert generation/writing), `gopkg.in/macaroon.v2`
  (binary macaroon (de)serialization), `google.golang.org/grpc`.
- **Depended on by**:
  - `darepod` (`rpc_security.go`, `server.go`, `outbound_clients.go`,
    `gateway_server.go`) — provisions the daemon's own TLS keypair,
    builds gRPC server creds, and attaches macaroon/TLS creds when
    darepod itself dials out to other services or proxies the REST
    gateway.
  - `sdk/walletdk` (`connect_grpc.go`, `client.go`) — builds TLS/macaroon
    dial options and an HTTP client for wallet daemon-kit RPC/REST
    clients.
  - `cmd/darepocli/darepoclicommands` (`client.go`) — attaches the CLI's
    macaroon to its gRPC connection to darepod.

## Invariants

- `MacaroonMetadataKey` must stay identical across every producer and
  consumer (gRPC metadata setters, REST gateway header allow-list in
  `darepod/gateway_server.go`, HTTP header attachment in
  `sdk/walletdk`). A mismatch silently drops the macaroon and either
  breaks auth or — worse — routes a request through as unauthenticated
  if the server-side check fails open.
- This package performs no permission or entity scoping: it moves
  bytes (macaroon blobs, TLS certs) but never inspects macaroon
  caveats or maps RPC methods to entities/actions. Callers (namely
  `darepod`'s auth interceptor) are solely responsible for verifying
  caveats and enforcing the entity/action permission map; treat any
  function here as an auth *transport*, not an auth *decision*.
- `EnsureTLSCert` only generates a new keypair when **both** cert and
  key are absent; if exactly one exists it errors out rather than
  silently overwriting, since regenerating one half of an existing
  pair would invalidate previously distributed client certs/macaroons
  bound to the old key.
- TLS credentials are pinned to a minimum of TLS 1.2
  (`tls.VersionTLS12`) on both server and client paths; do not lower
  this without a corresponding security review.
- Cert/key files are written with `0o600` and their parent directories
  with `0o700` — preserve these permissions when touching
  `EnsureTLSCert`, since the private key must not be world/group
  readable.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
