# scripts/verify-schema-registry

## Purpose

CI check that parses the `cmd/darepocli` Go source via `go/ast` and verifies
that the schema registry (`methodRegistry()`), MCP tool definitions
(`registerMCPTools()`), and cobra command tree are all in sync. Exits 0 on
success, 1 on drift. Run via `make schema-check`.

## Key Functions

- `main()` — Entry point: parses the CLI package, extracts all three sets, runs
  subset checks, and prints a summary or error list.
- `extractSchemaMethods(pkg)` — Walks `methodRegistry()` body looking for
  `Method` key-value fields in composite literals; returns sorted method names.
- `extractMCPToolNames(pkg)` — Walks all `mcp.AddTool[T](...)` calls and
  extracts the `Name` field from the `&mcp.Tool{}` argument; returns sorted
  tool names.
- `extractCobraLeafCommands(pkg)` — Walks all `new*Cmd()` functions to build a
  dotted command-path tree (e.g. `fees.estimate`); returns leaf commands that
  have a `RunE` handler; excludes schema-introspection and MCP server commands.
- `checkSubset(setA, setB, transform)` — Verifies every element of setA (after
  optional name transform) appears in setB. One-directional: setB may have extra
  entries (some schema methods are CLI-only).

## Relationships

- **Depends on**: `go/ast`, `go/parser` — reads source at the file system level,
  no imports from this repository.
- **Depended on by**: `make schema-check` Makefile target (CI gate).

## Invariants

- MCP tool names use `namespace_method` format; schema registry uses
  `namespace.method`. `mcpToSchema` converts by replacing the first underscore.
- Cobra command paths use dotted notation matching the schema registry key.
- The check is one-directional for MCP vs schema: every MCP tool must have a
  schema entry, but schema entries for sensitive CLI-only operations (wallet key
  material, etc.) need not have an MCP tool.

## Deep Docs

- [scripts/CLAUDE.md](../CLAUDE.md) — Parent scripts package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
