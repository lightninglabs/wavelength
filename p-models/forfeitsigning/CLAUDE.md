# p-models/forfeitsigning

## Purpose

Executable P model and trace schema for receive-side forfeit signing authority,
mailbox retry, broker replay, and restart behavior.

## Key Types

- `ForfeitSigningLifecycle` — publishes funded authority, admits exact requests,
  and retains the first accepted signature for each request id.
- `SigningRequiresPublishedExactAuthority` — rejects signer invocation before
  publication or for any binding mismatch.
- `BrokerReplayPreservesFirstSignature` — permits independently valid replay
  while preventing replacement of the first accepted signature set.
- `bridge.Trace` — loads the concrete lifecycle traces replayed by production
  package tests.

## Relationships

- **Depends on:** the P 3.0.4 checker and the production `sdk/swaps` and
  `waved` tests that consume the trace schema.
- **Depended on by:** `p-models/scripts/check.sh`.

## Invariants

- A mailbox request never supplies its own signing authority.
- Every vHTLC identity field matches the published funded binding before the
  signing oracle is reached.
- A valid replay cannot replace the broker's first accepted signature set.
- Durable authority survives restart; process-local authority does not.
- The request-derived counterexample must continue to find a bug.

## Deep Docs

- [README.md](README.md) — Requirements, abstractions, conformance matrix, and
  commands.
- [swap_system.md](../../docs/swap_system.md) — Receive flow and production
  signing boundary.
