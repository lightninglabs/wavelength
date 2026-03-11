# chainbackends

## Purpose

Concrete implementations of the `chainsource.ChainBackend` interface. Currently
provides `LNDBackend` wrapping lnd's chainntnfs for real-time chain
notifications and fee estimation.

## Key Types

- `LNDBackend` — Full-node backend wrapping lnd's chain notification and fee estimation interfaces.

## Relationships

- **Depends on**: `chainsource` (implements `ChainBackend` interface).
- **Depended on by**: `darepod` (instantiates backend).

## Invariants

- `LNDBackend` requires an lnd instance (local or remote via lndclient).
- Provides real-time notifications via lnd's chainntnfs package.

## Deep Docs

- [chainbackends/doc.go](doc.go) — Package overview.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
