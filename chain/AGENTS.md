# chain

## Purpose

Bitcoind/Bitcoin Core RPC utilities, including `BitcoindRPCClient` wrapping btcd
rpcclient with extended methods like `SubmitPackage` for v3 transaction package
relay.

## Relationships

- **Depends on**: nothing (low-level RPC wrapper).
- **Depended on by**: `harness` (test environment), `chainbackends` (chain integration).
