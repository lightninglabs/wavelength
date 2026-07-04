# rpcauth

## Purpose

Shared macaroon and TLS helpers for gRPC clients and servers across the
daemon and CLI: loading and hex-encoding macaroons, building gRPC
per-RPC macaroon credentials, generating/loading self-signed TLS
keypairs, and building TLS-backed gRPC and HTTP clients.

## Key Types

- `MacaroonMetadataKey` — gRPC metadata / HTTP header key (`"macaroon"`)
  carrying a serialized macaroon.
- `HexFromFile` — Reads a macaroon file and hex-encodes it.
- `DialOptionFromFile` — Builds a `grpc.DialOption` carrying per-RPC
  macaroon credentials from a macaroon file.
- `EnsureTLSCert` — Loads an existing TLS cert/key pair or generates a new
  self-signed pair.
- `ServerTLSCredentials` / `ClientTLSCredentials` — gRPC
  `credentials.TransportCredentials` for server and client sides.
- `HTTPClientForCert` — `*http.Client` rooted at a given cert (or system
  roots if empty).

## Relationships

- **Depends on**: `github.com/lightningnetwork/lnd/macaroons`,
  `github.com/lightningnetwork/lnd/cert`, `google.golang.org/grpc/credentials`,
  `gopkg.in/macaroon.v2`.
- **Depended on by**: `darepod` (`server.go`, `rpc_security.go`,
  `outbound_clients.go`, `gateway_server.go` — server TLS setup and
  macaroon-authenticated outbound clients), `sdk/walletdk` (`client.go`,
  `connect_grpc.go` — remote daemon connections), `cmd/darepocli`
  (`darepoclicommands/client.go` — CLI macaroon dial option).

## Invariants

- `EnsureTLSCert` refuses to proceed when only one of cert/key exists on
  disk (returns an error) rather than silently regenerating a keypair a
  client may already have pinned.
- Generated cert/key files are written `0o600`; their parent directories
  `0o700`.
- Both `ServerTLSCredentials` and `ClientTLSCredentials` pin
  `MinVersion: tls.VersionTLS12`.
