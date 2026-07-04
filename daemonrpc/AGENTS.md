# daemonrpc

## Purpose

Daemon gRPC API definitions: wallet operations, VTXO lifecycle (including
expiry classification and custom-policy refresh with participant-signature
callbacks), round queries, boarding, fee estimation, and VHTLC recovery
control. Proto source: `daemonrpc/daemon.proto`.

## Relationships

- **Depends on**: nothing (proto definitions).
- **Depended on by**: `darepod` (implements services), `cmd/darepocli` (uses generated clients).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`. All `*.pb.go`
  files (`daemon.pb.go`, `daemon.pb.gw.go`, `daemon_grpc.pb.go`,
  `daemon_mailboxrpc.pb.go`) are generated from `daemon.proto`/`daemon.yaml`.
