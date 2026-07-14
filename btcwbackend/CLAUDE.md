# btcwbackend

## Purpose

Lightweight in-process Bitcoin wallet backed by LND's btcwallet and a neutrino
(BIP 157/158) chain backend. Provides a self-contained SPV wallet that connects
directly to the Bitcoin P2P network without requiring an external Esplora server
or LND node.

## Key Types

- `Wallet` — Top-level wallet wrapping `walletcore.Wallet` with neutrino chain service, chain backend, and boarding adapter. Constructors: `New` (owns neutrino lifecycle) and `NewWithNeutrino` (reuses pre-started service).
- `NeutrinoService` — Manages the neutrino `ChainService`, its bbolt DB, and lifecycle. Pre-started by the daemon for early P2P sync.
- `ChainBackend` — Implements `chainsource.ChainBackend` using neutrino's native `ChainNotifier` for confirmation tracking, spend detection, fee estimation, and optional direct package relay via `chainbackends.PackageSubmitter`.
- `BoardingBackendAdapter` — Implements `wallet.BoardingBackend` and `wallet.OutputLeaser` by embedding `walletcore.BoardingBackendBase` and adding neutrino-backed `ListUnspent`, `GetTransaction`, `GetBlock`, `LeaseOutput`, and `ReleaseOutput`. Output leasing forwards to btcwallet's native lock table, casting `wallet.LockID` → `wtxmgr.LockID`.
- `Config` — Embeds `walletcore.Config` and adds neutrino-specific fields (peers, fee URL, persist filters) plus an optional `PackageSubmitter` for v3 parent+child relay.

## Relationships

- **Depends on**: `walletcore` (shared wallet/boarding base), `chainsource` (ChainBackend interface), `chainbackends` (PackageSubmitter interface), `wallet` (BoardingBackend interface), `build` (logging).
- **Depended on by**: `waved` (daemon startup and wallet lifecycle).

## Invariants

- `NewWithNeutrino` does not own the neutrino service lifecycle; the caller is responsible for stopping it. `New` owns the service and stops it on error or via `Wallet.Stop()`.
- Chain notifier cancel functions are wrapped in `sync.Once` to prevent double-cancel panics from lnd's notificationDispatcher.
- `SubmitPackage` requires a configured direct package submitter. Individual
  neutrino P2P broadcast is not equivalent to v3 package relay when the parent
  is non-relayable without its fee-paying child.
- Block cache size is in bytes (2 MiB default), not block count. This differs from neutrino's count-based API.
- Taproot script imports use `KeyScopeBIP0086` (via `walletcore.BoardingBackendBase`) to ensure btcwallet's block processing tracks credits correctly.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
