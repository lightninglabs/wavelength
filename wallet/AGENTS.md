# wallet

## Purpose

Manages on-chain boarding addresses (2-of-2 multisig with operator + CSV
timeout), monitors for confirmed boarding UTXOs, composes cooperative
intent packages, and gates round registration through the VTXO manager
admission APIs. The wallet actor owns VTXO selection and locking for
refresh, leave, OOR spend, and directed send flows.

## Key Types

- `Ark` — Main actor managing boarding addresses, UTXO enumeration, confirmation polling, admission forwarding, and VTXO selection/locking.
- `BoardingBackend` — Interface for wallet integration (key derivation, taproot import, ListUnspent). `GetTransaction` returns `*TxInfo` (containing tx, block hash, and block height).
- `TxInfo` — Struct wrapping a confirmed transaction with its block hash and block height. Returned by `BoardingBackend.GetTransaction`.
- `BoardingStore` — Interface for persisting boarding addresses and intents.
- `VTXOReader` — Read-only interface for loading VTXO descriptors by outpoint. Wallet uses this to build intent packages without importing `vtxo` directly.
- `VTXODescriptor` — Wallet-level VTXO descriptor (outpoint, amount, pkscript, tree, expiry). Avoids direct dependency on `vtxo.Descriptor`.
- `SelectedVTXO` — Describes a VTXO selected and locked for use as a transfer input (outpoint, amount, pkscript). Breaks the vtxo → round → wallet import cycle.
- `CreateBoardingAddressRequest` / `CreateBoardingAddressResponse` — Ask-request for deriving new address.
- `BlockEpochNotification` — Tell-message from chain source triggering UTXO polling.
- `BoardingUtxoConfirmedEvent` — Tell-message sent when a VTXO confirms.
- `BoardRequest` / `BoardResponse` — Ask-request from RPC to trigger boarding flow.
- `RefreshVTXOsRequest` — Ask-request to select VTXOs for refresh and compose intent package.
- `SelectAndLockVTXOsRequest` — Ask-request to select and lock VTXOs for OOR spend.
- `LeaveVTXOsRequest` — Ask-request to select VTXOs for cooperative leave.
- `CompleteSpendVTXOsRequest` — Tell-message to finalize spend and release locks.
- `UnlockVTXOsRequest` — Tell-message to release locked VTXOs on failure.
- `SendRecipient` — Describes a single directed send destination (pkscript, amount, recipient client key).
- `SendVTXOsRequest` / `SendVTXOsResponse` — Ask-request for in-round directed sends. Atomically selects and reserves VTXOs via `SelectAndReserveForfeitRequest`, builds forfeit + recipient VTXO intents, and registers with the round actor. Supports dry-run mode for previewing coin selection without committing.

## Relationships

- **Depends on**: `baselib/actor` (actor system), `chainsource` (block epoch notifications), `lib/actormsg` (VTXO manager admission types).
- **Depended on by**: `round` (boarding intents, types: `BoardingAddress`, `SelectedVTXO`), `db` (persistence), `darepod` (wiring).
- **Sends**:
  - → `round` (via registered notifier): `BoardingUtxoConfirmedEvent`
  - → `round` (via `lib/actormsg`): `TriggerBoardMsg` (VTXO amounts for boarding), `RegisterIntentMsg` (pre-composed cooperative intents with forfeits + VTXOs/leaves)
  - → `vtxo` manager (via `lib/actormsg`): `SelectAndReserveSpendRequest`, `ReleaseSpendRequest`, `CompleteSpendRequest`, `ReserveForfeitRequest`, `ReleaseForfeitRequest`, `SelectAndReserveForfeitRequest`
- **Receives**:
  - ← `chainsource`: `BlockEpochNotification` (triggers UTXO polling)
  - ← `round`: `RegisterConfirmationNotifierRequest`, `UnregisterConfirmationNotifierRequest`
  - ← API: `CreateBoardingAddressRequest`, `GetActiveBoardingAddressesRequest`, `GetBoardingBalanceRequest`, `RefreshVTXOsRequest`, `SelectAndLockVTXOsRequest`, `LeaveVTXOsRequest`, `BoardRequest`, `CompleteSpendVTXOsRequest`, `UnlockVTXOsRequest`, `SendVTXOsRequest`

## Invariants

- UTXO confirmation requires `MinBoardingConfs` (1) on-chain confirmations.
- `ListUnspent` queries are retried up to 10 times with 200ms delay (mitigates race between block epoch and wallet update, especially for neutrino backends).
- Notifier registration captures `minConf` parameter per actor; different actors can require different confirmation depths.
- Cooperative admission (refresh/leave) must reserve forfeit inputs through the VTXO manager before sending `RegisterIntentMsg` to the round actor.
- If round registration fails after successful admission, the wallet releases the forfeit reservation so VTXOs return to LiveState.
- Directed sends use `SelectAndReserveForfeitRequest` (cooperative forfeit path) rather than the OOR spend path. The wallet builds recipient VTXOs with the recipient's key as `OwnerKey` and derives a separate ephemeral `SigningKey` for MuSig2 tree construction.
- `VTXOReader` / `VTXODescriptor` / `SelectedVTXO` break the vtxo → round → wallet import cycle by providing wallet-level types that don't reference `vtxo.Descriptor` directly.
- Per-subsystem logging via `build.LoggerFromContext` (no global mutable loggers).

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
