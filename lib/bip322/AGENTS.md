# lib/bip322

## Purpose

BIP-322 message authentication implementation for Bitcoin/Ark protocol, enabling
intent-bound signatures over application payloads with block height validity
windows. Provides full-format signature construction, validation, and intent
metadata encoding.

## Key Types

- `TxSigner` — Interface for producing BIP-322 signatures (signs the to_spend
  and to_sign virtual transactions).
- `Intent` — Application payload with `ValidFrom`/`ValidUntil` block height
  range. TLV-encoded with domain tag `wavelength-bip322-intent`.
- `Sig` — Full-format BIP-322 signature (serialized to_sign transaction).
- `IntentAuthContext` — Complete intent-bound auth validation context (intent,
  message challenge, signature, proof prev outputs, chain height).
- `VerificationResult` — Validation outcome with state (Valid/Invalid/
  Inconclusive) and reason.
- `DefaultMaxProofInputs` (128) — The proof-of-funds input cap applied during
  validation, bounding worst-case script-engine work per auth package. It is
  exported because callers upstream of validation must size their own batches
  against the same budget rather than discovering the cap on rejection — the
  round actor reads it in `findRefreshRound` to decide how many forfeits one
  automatic-refresh cohort may add to a join.

## Relationships

- **Depends on**: (no internal repo imports; pure cryptographic library).
- **Depended on by**: `round` (join-round intent signing and BIP-322 auth
  validation via `join_auth.go`; `DefaultMaxProofInputs` as the cohort
  admission budget in `actor.go`).

## Invariants

- `ValidUntil` >= `ValidFrom` (or `ValidUntil` = 0 for no upper bound).
- Full-format only: signature is the serialized to_sign transaction.
- Max 128 additional inputs per validation (`DefaultMaxProofInputs`,
  overridable per call through the validation options).
- Height validation: signature valid only if `current >= ValidFrom` AND
  (`ValidUntil == 0` OR `current <= ValidUntil`).

## Deep Docs

- [lib/bip322/README.md](README.md) — BIP-322 implementation guide.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
