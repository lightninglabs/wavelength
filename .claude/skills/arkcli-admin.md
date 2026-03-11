---
name: arkcli-admin
description: "This skill provides context for working on the arkcli admin CLI code. It should be used when modifying cobra commands, schema registry, MCP server, output formatting, or any code in the cmd/arkcli/ directory. Triggers include working on CLI commands, adding new admin RPCs, schema introspection, or MCP tool definitions."
---

# arkcli — Ark Operator Admin CLI

## Overview

`arkcli` is the admin CLI for the Ark operator daemon (arkd). Output is
always JSON. All commands accept `--json` for raw proto-JSON request
payloads (agent-friendly path). Supports schema introspection and an MCP
stdio server for AI agent tool use.

## Global Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--rpcserver` | `localhost:8081` | Admin gRPC server address |
| `--tlscertpath` | | Path to TLS certificate |
| `--no-tls` | `false` | Connect without TLS (for regtest) |
| `--json` | | Raw proto-JSON request payload |

## Commands

### info — Server Status

```bash
arkcli info --no-tls
```

Returns server version, network, pubkey, block height.

### trigger-batch — Force a Round

```bash
arkcli trigger-batch --no-tls
```

### list-rounds — Query Round History

```bash
arkcli list-rounds --no-tls
arkcli list-rounds --status confirmed --limit 10 --no-tls
arkcli list-rounds --ndjson --no-tls           # one JSON object per line
arkcli list-rounds --fields id,status --no-tls # select specific fields
```

### list-vtxos — Query VTXO Inventory

```bash
arkcli list-vtxos --no-tls
arkcli list-vtxos --status active --limit 50 --no-tls
arkcli list-vtxos --ndjson --fields outpoint,status,value --no-tls
```

### vtxo-stats — Aggregate VTXO Statistics

```bash
arkcli vtxo-stats --no-tls
```

Returns total count, counts by status, total value locked (sats).

### list-clients — Connected Clients

```bash
arkcli list-clients --no-tls
arkcli list-clients --ndjson --fields client_id,status --no-tls
```

## Schema Introspection

```bash
arkcli schema                    # list all method names
arkcli schema info               # show schema for 'info' method
arkcli schema --all              # dump full registry as JSON
```

Schema output includes method name, description, parameters (name, type,
required, allowed values), request/response types, and whether `--dry-run`
and `--json` are supported.

## MCP Server

```bash
arkcli mcp serve --no-tls
```

Starts an MCP (Model Context Protocol) stdio server. Each admin RPC is
registered as a typed tool. An AI agent connects via stdio and can call
any admin operation as a structured tool invocation.

## Agent-Friendly JSON Input

Every command accepts `--json` for direct proto-JSON input, bypassing
bespoke flags:

```bash
arkcli list-rounds --no-tls --json '{
  "status_filter": "confirmed",
  "limit": 5
}'
```

The `--json` path always takes precedence over individual flags.

## Output Formatting

| Flag | Behavior |
|------|----------|
| (default) | Pretty-printed JSON object |
| `--ndjson` | Newline-delimited JSON (one object per line) |
| `--fields` | Comma-separated field names to include |
| `--json` (input) | Raw proto-JSON request body |

All output uses `protojson` with `UseProtoNames: true` and
`EmitUnpopulated: true` for consistent field naming.

## Regtest Quick Check

```bash
# Verify server is running
arkcli info --no-tls

# Check active clients
arkcli list-clients --no-tls

# View recent rounds
arkcli list-rounds --limit 5 --no-tls

# Get VTXO summary
arkcli vtxo-stats --no-tls

# Force a batch round
arkcli trigger-batch --no-tls
```
