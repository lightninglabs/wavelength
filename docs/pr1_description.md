# vtxo+round+wallet: purify VTXO FSM and move intent composition to wallet

## Summary

This PR refactors the VTXO lifecycle to cleanly separate concerns across the VTXO actor, round actor, and wallet. It is the first of three PRs in the VTXO refactor plan (`docs/vtxo_three_pr_execplan.md`).

**Phase A — Clean VTXO actor model (commits 1–7):**
- Rename `ExpiringState` → `UnilateralExitState`, `RefreshRequestedState` → `PendingForfeitState`
- Collapse `TriggerRefreshEvent`/`TriggerLeaveEvent` into a single `PendingForfeitEvent` — the VTXO FSM no longer distinguishes business intent (refresh vs leave)
- Route all round-bound signals (ForfeitRequest, ForfeitSignatureSubmission) through the VTXO manager instead of holding a direct round actor reference
- Remove `RefreshAcknowledgedEvent` (no-op ack that added complexity without value)
- Add manager-driven expiry liveness tests proving autonomous forfeit before critical expiry

**Phase B — Simplify round intent registration (commits 8–15):**
- Add `RegisterIntentRequest` as the single entry point for pre-composed intent packages
- Introduce `RegisterIntentMsg` (actormsg) for wallet → round communication without import cycles
- Add `VTXOReader` interface + `VTXOReaderFunc` adapter so the wallet can load VTXO data without importing the `vtxo` package
- Move intent composition (forfeit + VTXO/leave request pairing) from the round actor to the wallet
- Remove `TriggerVTXORefreshMsg`, `TriggerVTXOLeaveMsg`, `LeaveVTXORequest`, and all their handlers
- Carry locally-loaded forfeit amounts into round intents, eliminating fragile store lookups during registration
- Use `context.WithoutCancel` for local persistence that can outlive actor request contexts

## Motivation

The VTXO actor was entangled with product-level business logic (refresh vs leave), making it brittle to extend. The round actor owned intent composition, requiring it to import and understand VTXO internals. This created tight coupling and made the upcoming coin-selection work (PR 2) difficult.

After this PR:
- **VTXO actor** speaks pure lifecycle: Live → PendingForfeit → Forfeiting → Forfeited
- **Wallet** owns intent composition — it loads VTXOs, builds forfeit pairs, and sends a complete `RegisterIntentMsg`
- **Round actor** only validates and registers pre-composed intents

## Test plan

- [x] `make unit pkg=vtxo` — all VTXO FSM and manager tests pass
- [x] `make unit pkg=round` — round actor tests updated for `RegisterIntentRequest`
- [x] `make unit pkg=wallet` — wallet tests updated, nil-guard coverage added
- [x] `make build` — clean compilation, no import cycles
- [x] `make lint` — passes
