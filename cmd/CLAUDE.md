# cmd

## Purpose

Container for command-line entry points: `arkd` (daemon), `arkcli` (admin
CLI), `merge-sql-schemas` (schema utility), and `arktest` (itest-only manual
integration harness).

## Relationships

- **Depends on**: root `darepo` (server orchestration), `adminrpc` (CLI
  stubs), `harness` (`arktest` harness wiring).
- **Depended on by**: nothing (top-level binaries).
