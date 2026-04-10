# build

## Purpose

Logging infrastructure (context loggers, log levels, handlers), deployment mode
tags (dev/prod), and version information for the server binary.

## Relationships

- **Depends on**: nothing (leaf package).
- **Depended on by**: `cmd/arkd` (version, logging setup), root `darepo`
  (logger initialization).

## Notes

- The top-level `Makefile` builds three separate binaries — `arkd`,
  `arkcli`, and `merge-sql-schemas` — rather than wildcard-expanding
  `./cmd/...` into a single output path. If you add a new top-level command
  package, add an explicit build/install/release line for it alongside the
  existing three targets. Wildcard builds break cross-compilation as soon
  as the repo has more than one command package.
