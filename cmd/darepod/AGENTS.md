# cmd/darepod

## Purpose

Daemon entry point. Parses flags, initializes configuration, and starts the
`darepod.Server`.

## Key Flags

- `--maxoperatorfeesat` — Maximum operator fee (sats) the client will accept
  per seal-time quote under the #270 fee handshake. Zero is rejected at
  config-load time as explicit misconfiguration.

## Relationships

- **Depends on**: `darepod` (Server orchestrator).
- **Depended on by**: nothing (binary entry point).
