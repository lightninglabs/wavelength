# Lean Forfeit Signing Proof

## Purpose

Proves that the receive-side forfeit signing admission function cannot produce
a signing context without published authority and exact funded identity
bindings.

## Key Types

- `FundingBinding` — the immutable identity published by a funded receive
  session.
- `SigningRequest` — the identity carried by mailbox transport.
- `SigningContext` — the authority and request pair returned only by the safe
  admission function.
- `admit` — the executable safe gate mirrored by production.
- `legacyAdmit` — the request-derived rule retained only as a counterexample.

## Invariants

- `admit_sound` proves every admitted context came from published authority and
  satisfies every binding equality.
- Each single-field mutation is a compile-checked rejection example.
- The bridge vectors are emitted from the same Lean definitions and replayed
  against `receiveForfeitBindingGate.load` and
  `validateOutSwapForfeitSignaturePayload`.

## Deep Docs

- [`README.md`](README.md) — theorem scope, production mapping, and commands.
