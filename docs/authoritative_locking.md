# Authoritative Locking

This document defines the server-side locking model shared by rounds and OOR.

## Overview

The system uses two layers of coordination:

- client-local reservation for single-daemon race avoidance
- server-authoritative locking for cross-client correctness

Client reservations are useful, but they are not trusted by the server. The
only lock state that matters for correctness is the shared server-side
`vtxo.Locker`.

## Core Rules

Every authoritative lock flow follows the same order:

1. validate request structure
2. verify operation-specific authorization
3. mutate the shared locker
4. continue the protocol

If validation or authorization fails, the server must not mutate the shared
locker.

The locker itself stays deliberately simple. It only provides lock mutation by
outpoint and owner, and it does not interpret mailbox identity, PSBT proofs, or
protocol semantics. Those checks belong in the subsystem-specific boundary
above the locker.

## Canonical Owners

The server uses stable lock-owner names so retries and unlocks remain
idempotent:

- `round:<round-id>`
- `oor:<session-id>`

These owners are also threaded into VTXO lifecycle transitions, so spent-state
updates only succeed for the same operation that already holds the in-flight
claim.

## Rounds

Rounds authorize first via join or registration validation, then acquire locks
through the shared locker before the client is admitted to the round. The
implementation keeps the existing inline FSM flow for round joins, but it now
requires the shared `vtxo.Locker` and no longer falls back to direct
`VTXOStore` lock mutation.

That means rounds and OOR still share one authoritative lock owner model even
though the two packages use slightly different control-flow styles. For rounds,
the important invariant is not "all lock calls happen through an outbox"; it is
"every round-owned VTXO claim goes through the same shared locker that OOR
uses."

## OOR

OOR validates before lock. Submit handling first checks package structure and
rebuilds the request against authoritative VTXO state. It then verifies owner
proof by requiring the Ark input for each checkpoint to use the standard
collaborative owner leaf and to carry a valid owner signature for that leaf.

Only after those checks succeed does OOR acquire the shared authoritative lock.
If submit validation fails, the session terminates without emitting a lock or
unlock side effect.

## Failure And Recovery

Before the point of no return, failure paths should release authoritative locks
using the same owner that acquired them. After the point of no return, the
operation transitions through owner-aware in-flight and spent states instead of
unlocking.

The practical goal is that rounds and OOR always agree on lock ownership across
normal execution, retries, and crash or restart windows.
