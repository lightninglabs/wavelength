# cmd

## Purpose

Container for command-line entry points: `arkd` (daemon), `arkcli` (admin CLI),
and `merge-sql-schemas` (schema utility).

## Relationships

- **Depends on**: root `darepo` (server orchestration), `adminrpc` (CLI stubs).
- **Depended on by**: nothing (top-level binaries).
