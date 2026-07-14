# rpcauth

## Purpose

Shared macaroon and TLS helpers for securing gRPC/REST connections between
waved, its CLI, and SDK clients: loading/serving macaroons and
generating/loading self-signed TLS cert/key pairs.

## Key Types

- `MacaroonMetadataKey` — gRPC metadata/HTTP header key carrying the
  serialized macaroon.
- `DialOptionFromFile` / `HexFromFile` — build a macaroon `grpc.DialOption`
  or hex-encode a macaroon file for client use.
- `EnsureTLSCert` — loads an existing TLS cert/key pair or generates a
  self-signed one if neither exists.
- `ServerTLSCredentials` / `ClientTLSCredentials` / `HTTPClientForCert` —
  build gRPC/HTTP transport credentials from a cert/key pair.

## Relationships

- **Depends on**: `github.com/lightningnetwork/lnd/macaroons` (macaroon
  credentials), `github.com/lightningnetwork/lnd/cert` (self-signed cert
  generation).
- **Depended on by**: `waved` (server + gateway TLS/macaroon wiring),
  `cmd/wavecli` (client auth), `sdk/walletdk` (SDK gRPC client connection).

## Invariants

- `EnsureTLSCert` errors on a partial keypair (only one of cert/key present)
  rather than silently regenerating — a missing key file must never trigger
  a fresh cert that invalidates a still-present key, or vice versa.
- TLS transport config always pins `MinVersion: tls.VersionTLS12`.
- Cert/key files are written `0o600` and their parent dirs `0o700`.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
