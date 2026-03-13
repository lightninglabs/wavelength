# wallet

## Purpose

Manages on-chain boarding addresses (2-of-2 multisig with operator + CSV
timeout), monitors for confirmed boarding UTXOs, and composes intent packages
for round participation (refresh, leave). The wallet owns intent composition:
it loads VTXO descriptors, builds forfeit+output pairings, and sends
pre-composed `RegisterIntentMsg` to the round actor.

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
  - → `round` (via `lib/actormsg`): `RegisterIntentMsg` (pre-composed intent package with forfeits + VTXOs/leaves), `TriggerBoardMsg` (VTXO amounts for boarding)
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
