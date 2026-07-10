# cmd/darepocli

## Purpose

Binary entry point for the `darepocli` CLI. `main` is a thin wrapper: it
builds the root cobra command from `darepoclicommands`, executes it, and
maps any returned error onto a semantic process exit code (2=invalid args,
3=auth, 4=not found, 10=dry-run) so scripting agents can branch on failure
category without parsing stderr prose.

## Relationships

- **Depends on**: `cmd/darepocli/darepoclicommands` (root command, all
  subcommands, exit-code table); `cmd/darepocli/internal/gen-devrpc`
  (code-gen tool, not linked into the binary).
- **Depended on by**: nothing (binary entry point).

## Invariants

- Any error already printed via `darepoclicommands.PrintError` (checked
  with `ErrorWasPrinted`) must not be printed again here; `main` only
  renders the fallback envelope for errors that reach it unprinted.

## Deep Docs

- [docs/daemon_cli_guide.md](../../docs/daemon_cli_guide.md) — CLI reference.
