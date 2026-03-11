# rpc

## Purpose

Client-side RPC message definitions in sub-packages: `rpc/roundpb` (round
protocol messages) and `rpc/oorpb` (OOR transfer messages).

## Relationships

- **Depends on**: nothing (proto definitions).
- **Depended on by**: `round`, `oor`, `serverconn` (uses generated message types).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
