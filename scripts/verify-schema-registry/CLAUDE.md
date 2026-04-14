# scripts/verify-schema-registry

## Purpose

CI check tool that parses the `cmd/arkcli` source using Go AST and verifies
that the schema registry (`methodRegistry()`), MCP tool definitions
(`registerMCPTools()`), and cobra command tree are all in sync. Run via
`go run scripts/verify-schema-registry/main.go`; exits 0 if in sync, 1 on
drift.

## Key Types

- `main` — Entry point: parses the arkcli package, runs three cross-checks,
  and reports any drift.
- `extractSchemaMethods` — AST walker that collects `Method` field values
  from `methodRegistry()` composite literals.
- `extractMCPToolNames` — AST walker that collects `Name` fields from
  `mcp.AddTool` calls.
- `extractCobraLeafCommands` — AST walker that builds cobra command paths by
  walking `new*Cmd()` functions and `AddCommand` call trees.
- `checkSubset` — Verifies that every name in set A (after optional transform)
  exists in set B; used for MCP→schema and cobra→schema checks.

## Relationships

- **Depends on**: `cmd/arkcli` source (read via `go/ast`, `go/parser`).
- **Depended on by**: Makefile / CI (`make ast-lint` or equivalent CI step).

## Invariants

- The MCP→schema check is one-directional: every MCP tool must have a schema
  entry, but schema entries may exist without a matching MCP tool (CLI-only).
- The cobra→schema check excludes `schema` and `mcp` sub-commands (meta-commands
  not backed by gRPC methods).
- MCP tool names use underscores; schema method names use hyphens —
  `mcpToSchema` normalizes the conversion.
- Relies on naming conventions: schema methods live in `methodRegistry()`,
  MCP tools are registered via `mcp.AddTool`, and cobra commands are built
  by `new*Cmd()` functions.

## Deep Docs

- [scripts/CLAUDE.md](../CLAUDE.md) — Parent scripts package context.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
