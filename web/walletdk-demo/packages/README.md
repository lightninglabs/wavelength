# walletdk web packages

This workspace keeps the browser demo honest by making it consume the same
SDK shape that another wallet application would use.

- `@lightninglabs/walletdk-core` contains the stable TypeScript DTOs,
  errors, event model, and `WalletDKClient` interface.
- `@lightninglabs/walletdk-wasm-web` loads the Go `walletdk` WASM runtime,
  SQLite OPFS bridge assets, and exposes the `WalletDKClient` interface.
- `@lightninglabs/walletdk-react` provides `WalletDKProvider` plus hooks for
  React wallets.

The default web adapter currently hosts Go WASM on the main browser thread
because the wallet seed path still uses `localStorage`, which is unavailable
inside a Web Worker. `walletdk-worker.js` remains as the worker protocol
prototype for the point where the encrypted seed store no longer depends on
window-only APIs.
