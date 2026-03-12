# build

## Purpose

Logging infrastructure (context loggers, log levels, handlers), deployment mode
tags (dev/prod), and version information for the server binary.

## Relationships

- **Depends on**: nothing (leaf package).
- **Depended on by**: `cmd/arkd` (version, logging setup), root `darepo` (logger initialization).
