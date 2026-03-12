# arkrpc

## Purpose

Server-side gRPC service definitions (ArkService, IndexerService) with generated
Go stubs. Proto source: `arkrpc/ark.proto`, `arkrpc/indexer.proto`.

## Relationships

- **Depends on**: nothing (proto definitions).
- **Depended on by**: `indexer`, `darepod`, `serverconn` (uses generated clients).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
