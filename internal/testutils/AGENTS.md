# internal/testutils

## Purpose

Shared test helpers: deterministic key/signature generation and a Script
engine execution asserter, used by unit tests across the repo.

## Key Types

- `CreateKey(index int32) (*btcec.PublicKey, input.Signer)` — Deterministic
  key pair (from `index`) plus a mock `input.Signer` for it.
- `TestSchnorrSignature(t, seed string) *schnorr.Signature` — Deterministic
  Schnorr signature over a fixed test message, keyed off `seed`.
- `AssertEngineExecution(t, testNum int, valid bool, newEngine func() (*txscript.Engine, error))` —
  Runs a `txscript.Engine`, asserting it validates (or fails) as expected;
  on mismatch, single-steps the VM and dumps stack/disassembly for debugging.

## Relationships

- **Depends on**: `btcec`/`schnorr`/`txscript` (btcd), `lnd/input` (mock
  signer), `testify/require`.
- **Depended on by**: test files in `vtxo`, `round`, `lib/tree`, `lib/tx`,
  `lib/arkscript`, `vhtlcrecovery/unrollpolicy`.

## Invariants

- Key/signature generation must stay deterministic (fixed seeds/indices) so
  golden-value and reproducibility-sensitive tests do not flake.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
