# lib/tx/psbtutil

## Purpose

Small helpers for encoding, decoding, and manipulating PSBTs. Intentionally
"dumb": only serialize/parse bytes and attach signatures. Does not validate
transaction semantics — callers run appropriate protocol validators.

## Key Functions

- `Serialize` / `Parse` — Encode/decode PSBT packets to/from raw bytes.
- `EncodeBase64` / `DecodeBase64` — Standard (not URL-safe) base64 PSBT
  encoding for tool compatibility.
- `AddTaprootScriptSpendSig` — Adds or replaces a taproot script-path spend
  signature, keyed by (x-only pubkey, leaf hash).
- `AddTapLeafScript` — Idempotent tapscript leaf attachment to PSBT input.

## Relationships

- **Depends on**: `lib/arkscript` (`SpendInfo` for taproot helpers).
- **Depended on by**: `lib/tx/oor` (package marshaling), `oor` (signing flow,
  snapshot/artifact codecs), `db` (artifact store persistence), `unroll`
  (proof assembly), `rpc/oorpb` / `waved` (wire payload conversion).

## Invariants

- Base64 encoding uses standard alphabet (not URL-safe) for tool compatibility.
- Serialization is symmetric: `Parse(Serialize(pkt))` round-trips.
- `AddTaprootScriptSpendSig` replaces existing signature with same key.
- `AddTapLeafScript` is idempotent: adding the same leaf twice is a no-op.

## Deep Docs

- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
