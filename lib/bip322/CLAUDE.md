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
  range. TLV-encoded with domain tag `darepo-bip322-intent`.
- `Sig` — Full-format BIP-322 signature (serialized to_sign transaction).
- `IntentAuthContext` — Complete intent-bound auth validation context (intent,
  message challenge, signature, proof prev outputs, chain height).
- `VerificationResult` — Validation outcome with state (Valid/Invalid/
  Inconclusive) and reason.

## Relationships

- **Depends on**: (no internal repo imports; pure cryptographic library).
- **Depended on by**: `round` (join-round intent signing and BIP-322 auth
  validation, via `join_auth.go`).

## Invariants

- `ValidUntil` >= `ValidFrom` (or `ValidUntil` = 0 for no upper bound).
- Full-format only: signature is the serialized to_sign transaction.
- Max 128 additional inputs per validation (proof-of-funds limit).
- Height validation: signature valid only if `current >= ValidFrom` AND
  (`ValidUntil == 0` OR `current <= ValidUntil`).

## Deep Docs

- [lib/bip322/README.md](README.md) — BIP-322 implementation guide.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
