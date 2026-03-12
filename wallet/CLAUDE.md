# wallet

## Purpose

Manages on-chain boarding addresses (2-of-2 multisig with operator + CSV
timeout) and monitors for confirmed boarding UTXOs, notifying the round actor
when new boarding opportunities are available.

## Key Types

- `Ark` — Main actor managing boarding addresses, UTXO enumeration, and confirmation polling.
- `BoardingBackend` — Interface for wallet integration (key derivation, taproot import, ListUnspent).
- `BoardingStore` — Interface for persisting boarding addresses and intents.
- `CreateBoardingAddressRequest` / `CreateBoardingAddressResponse` — Ask-request for deriving new address.
- `BlockEpochNotification` — Tell-message from chain source triggering UTXO polling.
- `BoardingUtxoConfirmedEvent` — Tell-message sent when a VTXO confirms.
- `BoardRequest` / `BoardResponse` — Ask-request from RPC to trigger boarding flow (balance check, fee deduction, round registration).

## Relationships

- **Depends on**: `baselib/actor` (actor system), `chainsource` (block epoch notifications).
- **Depended on by**: `round` (boarding intents), `db` (persistence), `darepod` (wiring).
- **Sends**:
  - → `round` (via registered notifier): `BoardingUtxoConfirmedEvent`
  - → `round` (via `lib/actormsg`): `TriggerBoardMsg` (VTXO amounts for boarding)
  - → `vtxo`: `TriggerRefreshEvent`, `TriggerLeaveEvent`
- **Receives**:
  - ← `chainsource`: `BlockEpochNotification` (triggers UTXO polling)
  - ← `round`: `RegisterConfirmationNotifierRequest`
  - ← API: `CreateBoardingAddressRequest`, `GetActiveBoardingAddressesRequest`, `GetBoardingBalanceRequest`, `RefreshVTXOsRequest`, `SelectAndLockVTXOsRequest`, `LeaveVTXOsRequest`, `BoardRequest`

## Invariants

- UTXO confirmation requires `MinBoardingConfs` (1) on-chain confirmations.
- `ListUnspent` queries are retried up to 5 times with 200ms delay (mitigates race between block epoch and wallet update).
- Notifier registration captures `minConf` parameter per actor; different actors can require different confirmation depths.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
