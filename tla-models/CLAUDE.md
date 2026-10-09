# tla-models

## Purpose

Executable TLA+ specifications for crash-recovery and cross-transaction
properties whose complete state space is small enough for exhaustive TLC
model checking.

## Key Models

- `durablehandoff/` models a transactional durable-message handoff, rollback
  and retry, restart discovery of delayed and leased rows, and retention of a
  consumer after its domain state becomes terminal.
- `scripts/check.sh` runs the production profile and proves that four
  deliberately weakened profiles still produce their expected
  counterexamples.

## Invariants

- A positive configuration and its counterexample configurations use the same
  transition system. Counterexamples change constants only.
- A negative configuration that unexpectedly passes is a test failure.
- Keep the model finite and exhaustive. Do not replace the checked safety
  properties with sampled simulations.

## Deep Docs

- [`README.md`](README.md) describes the model boundary and local TLC command.
- [`durablehandoff/README.md`](durablehandoff/README.md) maps each action and
  invariant to the durable handoff lifecycle.
- [`docs/durable_actor_architecture.md`](../docs/durable_actor_architecture.md)
  describes the implementation being modeled.
