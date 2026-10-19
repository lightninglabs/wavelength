# baselib/tlvutil

## Purpose

Shared helpers that collapse the repetitive `lnd/tlv` stream scaffolding
(`NewStream` + `Encode` / `DecodeWithParsedTypes`) used by durable actor
message and snapshot codecs into a records slice plus a single call. The
helpers are thin wrappers that produce byte-identical output to the inlined
form they replace.

## Key Types

- `EncodeRecords` / `EncodeRecordsToBytes` — Build a TLV stream from records
  and write it to a writer or a fresh `[]byte`.
- `DecodeRecords` / `DecodeRecordsFromBytes` — Decode into records and return
  the parsed `tlv.TypeMap` so callers can detect which optional records were
  present (unknown record values are retained in the map).
- `DecodeRecordsDiscardUnknown` — Decode known records, stream unknown values
  to `io.Discard`, and return no type map (plain `tlv.Stream.Decode`
  behavior).
- `OutPointRecord` — Static 36-byte TLV record for `wire.OutPoint`
  (32-byte hash + 4-byte little-endian index).
- `ErrInvalidOutPointLength` — Sentinel returned when an outpoint record's
  declared length is not exactly 36 bytes.

## Relationships

- **Depends on**: `lnd/tlv` (stream and record primitives),
  `btcd/wire/v2` and `btcd/chainhash/v2` (outpoint layout). No wavelength
  imports.
- **Depended on by**: `baselib/actor` (ask-response and restart codecs),
  `ledger`, `oor`, `unroll`, `unrollplan`, `serverconn` (durable message,
  snapshot, and query codecs).
- **Sends**: none (pure codec library).
- **Receives**: none.

## Invariants

- Wire and on-disk format must not change: helpers must stay byte-for-byte
  equivalent to the hand-written `NewStream`/`Encode` form they replaced.
  Durable mailbox rows and snapshots written by older binaries must still
  decode.
- `OutPointRecord` preserves the ledger outpoint record format (hash bytes
  then little-endian index). Other outpoint encodings in the repo may use a
  different byte order; do not swap one for the other without a migration.
- `OutPointRecord` decodes directly into the passed `*wire.OutPoint`; it must
  be non-nil and stay valid for the lifetime of the encode/decode call.
- The decode helpers add no size limits. Callers that read untrusted input
  must bound it themselves.
- Types `lnd/tlv` handles natively (`uint*`, `[32]byte`, `[33]byte`,
  `*btcec.PublicKey`, `[64]byte`, `[]byte`) should use
  `tlv.MakePrimitiveRecord` directly; only add a helper here for a recurring
  field with no native primitive.

## Deep Docs

- [docs/durable_actor_quickstart.md](../../docs/durable_actor_quickstart.md) —
  `TLVMessage` codec conventions.
- [baselib/CLAUDE.md](../CLAUDE.md) — Parent module overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
