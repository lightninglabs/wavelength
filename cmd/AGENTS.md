# cmd

## Purpose

Entry points for the daemon (`cmd/darepod`), CLI client (`cmd/darepocli`),
and supporting build tools: `cmd/merge-sql-schemas` (concatenates sqlc
migration files for embedding), `cmd/protoc-gen-mailboxrpc` (protoc plugin
generating the mailbox-actor RPC glue), and `cmd/walletdk-wasm` (js/wasm
build target exposing `sdk/walletdk` to browser JS).

## Relationships

- **Depends on**: `darepod` (daemon orchestrator), `daemonrpc` (gRPC API
  definitions), `sdk/walletdk` (wasm target only).
- **Depended on by**: nothing (top-level binaries).

## Deep Docs

- [docs/daemon_cli_guide.md](../docs/daemon_cli_guide.md) — Installation and CLI reference.
