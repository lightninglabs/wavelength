# scripts/verify-schema-registry

## Purpose

CI check that parses the `cmd/darepocli` Go source via `go/ast` and verifies
that the schema registry (`methodRegistry()`), MCP tool definitions
(`registerMCPTools()`), and cobra command tree are all in sync. Exits 0 on
success, 1 on drift. Run via `make schema-check`.

## Key Functions

- `main()` — Parses the CLI package, extracts all three sets, filters
  schema-introspection/MCP-server commands out of the cobra set, runs both
  subset checks, and prints a summary or error list.
- `extractSchemaMethods(pkg)` — Walks `methodRegistry()` body looking for
  `Method` key-value fields in composite literals; returns sorted method names.
- `extractMCPToolNames(pkg)` — Walks all `mcp.AddTool[T](...)` calls and
  extracts the `Name` field from the `&mcp.Tool{}` argument; returns sorted
  tool names.
- `extractCobraLeafCommands(pkg)` — Walks all `new*Cmd()` functions, builds a
  parent/child tree from `AddCommand` calls starting at `newRootCmd`, and
  returns dotted `Use`-field paths (e.g. `fees.estimate`) for commands that
  set `RunE`.
- `checkSubset(nameA, setA, nameB, setB, transform)` — Verifies every element
  of setA (after `transform`) appears in setB; one-directional, setB may have
  extra entries.

## Relationships

- **Depends on**: `go/ast`, `go/parser` — reads source at the file system level,
  no imports from this repository.
- **Depended on by**: `make schema-check` Makefile target (CI gate).

## Invariants

- MCP tool names use `namespace_method` format; schema registry uses
  `namespace.method`. `mcpToSchema` converts by replacing the first underscore.
- `schemaToCobra` is the identity function: cobra `Use` paths must already
  match the schema registry key verbatim.
- Both checks are one-directional: every MCP tool must map to a schema entry,
  and every RPC cobra command (after excluding `schema`/`mcp`-prefixed meta
  commands) must map to a schema entry. Schema entries may have no MCP/cobra
  counterpart (e.g. sensitive CLI-only wallet operations).

## Deep Docs

- [scripts/CLAUDE.md](../CLAUDE.md) — Parent scripts package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
