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
- `DefaultMaxProofInputs = 128` — The proof-of-funds input ceiling applied by
  `defaultValidateAuthOptions`. It is exported (not just an internal default)
  so a *producer* can refuse to assemble a package the verifier would reject:
  `round.findRefreshRound` budgets automatic refresh cohorts against it. Its
  value is unchanged, so existing verifiers interoperate with no protocol
  migration.

## Relationships

- **Depends on**: (no internal repo imports; pure cryptographic library).
- **Depended on by**: `round` (join-round intent signing and BIP-322 auth
  validation, via `join_auth.go`; plus `DefaultMaxProofInputs` as the
  refresh-cohort admission budget in `actor.go`).

## Invariants

- `ValidUntil` >= `ValidFrom` (or `ValidUntil` = 0 for no upper bound).
- Full-format only: signature is the serialized to_sign transaction.
- Max `DefaultMaxProofInputs` (128) additional inputs per validation
  (proof-of-funds limit). Callers that build auth packages must respect the
  same bound: a package over the limit cannot be made to pass by retrying.
- Height validation: signature valid only if `current >= ValidFrom` AND
  (`ValidUntil == 0` OR `current <= ValidUntil`).

## Deep Docs

- [lib/bip322/README.md](README.md) — BIP-322 implementation guide.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
