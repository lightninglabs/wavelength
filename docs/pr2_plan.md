# PR 2: Actor-Owned VTXO Admission for Spend and Cooperative Forfeit

## Goal

Revise the earlier "manager-owned reservations first" idea into a stronger
model: the VTXO FSM state is the single source of truth for availability.

After PR 2:

- a VTXO claimed for OOR spending is in `SpendingState`
- a VTXO claimed for cooperative consumption is in `PendingForfeitState`
- the manager coordinates admission, but does not own a separate lock set
- the wallet must acquire VTXO admission before starting either an OOR spend
  or a cooperative round intent

This is an intentional change from the earlier roadmap. The motivation is to
avoid split authority where the manager says "locked" while the VTXO still
thinks it is `Live` and can independently move toward forfeit.

## Design Rules

- VTXO actor state is authoritative for availability.
- Admission happens before execution:
  OOR spend before OOR session start, cooperative forfeit before
  `RegisterIntentMsg`.
- The manager is the orchestrator for admission, rollback, and completion.
- The round actor should not be the first component to discover that a VTXO
  is unavailable.
- OOR completion must finalize through VTXO actors, not by writing `Spent`
  directly in an unrelated handler.
- No separate in-memory reservation set.

## FSM Model

### New and Revised States

Add two explicit spend-related states to the VTXO FSM:

- `SpendingState`:
  non-terminal, claimed for OOR spending
- `SpentState`:
  terminal, OOR spend completed

The effective model becomes:

```text
Live
  -> Spending
  -> PendingForfeit
  -> Forfeiting
  -> Forfeited
  -> Spent
  -> UnilateralExit
  -> Failed
```

`PendingForfeitState` remains the cooperative-claim state. The difference in
PR 2 is that wallet/manager admission moves a VTXO into `PendingForfeitState`
before round registration, and releases it back to `LiveState` if round
registration fails.

### Persisted Status

`SpendingState` must be persisted as `VTXOStatusSpending`.

This is required if actor state is the source of truth. A restart must not
silently turn a claimed spend back into `Live`. `VTXOStatusSpent` already
exists and should map to an explicit terminal `SpentState`.

### New Events

- `SpendReserveEvent`:
  `Live -> Spending`
- `SpendReleasedEvent`:
  `Spending -> Live`
- `SpendCompletedEvent`:
  `Spending -> Spent`
- `ForfeitReleasedEvent`:
  `PendingForfeit -> Live`

`PendingForfeitEvent` remains the event that claims a VTXO for cooperative
consumption. In PR 2 it becomes a manager-driven admission event rather than a
round-side post-registration notification.

### Conflict Rules

| Current State     | SpendReserveEvent | PendingForfeitEvent |
|-------------------|-------------------|---------------------|
| Live              | -> Spending       | -> PendingForfeit   |
| Spending          | reject            | reject              |
| PendingForfeit    | reject            | idempotent/no-op    |
| Forfeiting        | reject            | reject              |
| Spent/Forfeited   | reject            | reject              |
| UnilateralExit    | reject            | reject              |
| Failed            | reject            | reject              |

`SpendReleasedEvent` is only valid from `SpendingState`.

`ForfeitReleasedEvent` is only valid from `PendingForfeitState`.

### Expiry Policy in SpendingState

`SpendingState` must not ignore expiry.

If a VTXO is claimed for spend but reaches critical expiry before the spend
completes, the safety rule must still win:

- critical expiry triggers the existing unilateral-exit path
- the VTXO transitions to `UnilateralExitState`
- the OOR operation later fails or releases when it discovers the claim is
  gone

PR 2 should not introduce a state that can suppress expiry safety.

## Admission Flows

### OOR Spend Admission

Spend selection remains manager-coordinated, but the claim itself lives in
the VTXO actors.

```text
RPC/Wallet -> Manager: SelectAndReserveSpendRequest(target)
Manager:
  1. list candidate VTXOs from store
  2. filter to persisted `live` candidates only
  3. run largest-first selection
  4. Ask each selected VTXO actor to process SpendReserveEvent
  5. if any reservation fails:
       Ask SpendReleasedEvent on already-reserved VTXOs
       return error
  6. return selected VTXOs
```

This is atomic from the caller's perspective: either all selected VTXOs enter
`SpendingState`, or they are rolled back to `LiveState`.

### Cooperative Admission

This is the main architectural change relative to PR 1.

Today the wallet builds an intent package and the round actor registers it,
then the round notifies VTXO actors with `PendingForfeitEvent`.

PR 2 should change that to:

```text
Wallet:
  1. load target VTXOs
  2. Ask manager to reserve them for cooperative use
  3. if reservation succeeds, build/send RegisterIntentMsg to round
  4. if round registration fails, release the VTXOs back to Live
```

Concrete manager flow:

```text
Wallet -> Manager: ReserveForfeitRequest(outpoints)
Manager:
  1. verify all outpoints refer to tracked live VTXOs
  2. Ask each selected VTXO actor to process PendingForfeitEvent
  3. if any reservation fails:
       Ask ForfeitReleasedEvent on already-claimed VTXOs
       return error
  4. return success
```

If the later `RegisterIntentMsg` call fails, the wallet must send
`ReleaseForfeitRequest(outpoints)` so the manager asks each actor to process
`ForfeitReleasedEvent`.

This means the round actor is no longer responsible for making VTXOs
`PendingForfeit`. It should assume the wallet/manager already admitted the
package inputs.

### Why This Is Necessary

If cooperative admission still happens only after round registration, then
`SpendingState` rejection happens too late: the round may already have a
registered intent for a VTXO that is spending. That is the exact split-brain
behavior PR 2 is supposed to remove.

## OOR Completion and Release

### Failure / Cancellation

On any OOR failure after spend admission succeeds, the caller must release the
claim through the manager:

```text
Wallet/RPC/OOR -> Manager: ReleaseSpendRequest(outpoints)
Manager -> VTXO actors: SpendReleasedEvent
VTXO actors: Spending -> Live
```

This includes:

- selection succeeded but transfer input assembly failed
- OOR session failed before durable completion
- explicit cancellation paths

### Successful Completion

PR 2 must remove the current split authority in the OOR completion path.

Today the outgoing OOR persistence handler writes `VTXOStatusSpent` directly
to the store. That is incompatible with actor-owned availability.

Instead:

- when the OOR FSM reaches the durable "mark inputs spent" phase, the local
  handler should ask the manager to complete the spend for those outpoints
- the manager should Ask each VTXO actor to process `SpendCompletedEvent`
- each VTXO actor should persist `VTXOStatusSpent` through its own outbox,
  transition to `SpentState`, and emit termination notification
- only after that succeeds should the OOR FSM emit its follow-up success event

The important rule is:

`Spent` is produced by the VTXO actor transition, not by a direct store write
that bypasses the actor.

## Restart and Recovery

`SpendingState` should be recovered from persisted `VTXOStatusSpending`.

Do not auto-clear spending claims on restart.

Rationale:

- OOR sessions are durable and already track their input outpoints
- clearing claims on restart would violate the "actor state is source of
  truth" rule
- a resumed OOR session can later release or complete the claim by outpoint

If startup ever finds a `Spending` VTXO with no corresponding resumable OOR
session, treat that as a repair condition to surface explicitly. Do not
silently downgrade it to `Live` in PR 2.

`PendingForfeitState` recovery stays as it works today: the VTXO remains
claimed for cooperative consumption until the round continues or an explicit
release is issued.

## Manager API Shape

Exact names can change, but PR 2 should expose concepts equivalent to:

- `SelectAndReserveSpendRequest`
- `SelectAndReserveSpendResponse`
- `ReleaseSpendRequest`
- `CompleteSpendRequest`
- `ReserveForfeitRequest`
- `ReleaseForfeitRequest`

The manager should validate that every explicitly named outpoint is known and
tracked. Unknown outpoints should be rejected, not silently accepted.

For spend selection, the manager may continue to use largest-first coin
selection.

## Wallet and Round Responsibilities

### Wallet

The wallet remains the place that knows why the user is consuming VTXOs.

For PR 2 it gains one new duty: acquire actor admission before starting the
operation.

Spend flow:

- ask manager to select and reserve spend inputs
- start OOR flow
- release or complete later through manager

Refresh / leave flow:

- decide target outpoints
- ask manager to reserve for cooperative consumption
- send `RegisterIntentMsg`
- release on registration failure

### Round

The round actor should keep doing one job: register and run the round FSM.

PR 2 should remove the assumption in `handleRegisterIntent` that the round is
the component that first marks VTXOs pending. That pending state should already
exist by the time the round registers the package.

The round actor should still send concrete `ForfeitRequestEvent` and
`ForfeitConfirmedEvent` later in the lifecycle.

## PR 168 Review Comments - Carry Forward

The review comments from PR #168 still matter, but the answers change under
this stronger model:

1. Error paths must release claims.
   Every path after successful admission must drive either release or
   completion.

2. The VTXOs themselves need to know they are locked.
   Yes. That is the point of `SpendingState` and manager-driven
   `PendingForfeitState`.

3. Add focused tests for manager admission methods.
   Required.

4. Only reserve actual tracked VTXO outpoints.
   Required.

5. Reduce actor Ask/Await/type-assert boilerplate when touching those paths.
   Still worth doing. PR 2 will add enough manager calls that a local helper
   is justified.

6. Do not rely on round-side special cases to fix the race.
   Correct. Admission must happen before round registration.

7. Auto-timeout on stuck spends can stay deferred.
   It is optional. Persisted actor-owned claims are the main correctness
   mechanism.

## Commit Plan

### Commit 1: Extend the VTXO FSM for explicit spending

This commit should:

- add `SpendingState` and `SpentState`
- add `VTXOStatusSpending`
- map `VTXOStatusSpent` to `SpentState`
- add `SpendReserveEvent`, `SpendReleasedEvent`, `SpendCompletedEvent`,
  and `ForfeitReleasedEvent`
- add transition tests for:
  - `Live -> Spending`
  - `Spending -> Live`
  - `Spending -> Spent`
  - `PendingForfeit -> Live`
  - conflict rejection in both directions
  - critical expiry behavior from `SpendingState`

Reviewer should be able to say:

    "availability is now represented directly in VTXO actor state"

### Commit 2: Add manager admission and rollback APIs

This commit should:

- add manager messages for spend selection/reserve/release/complete
- add manager messages for cooperative reserve/release
- add largest-first selection in the manager
- use Ask/Await with VTXO actors for reservation attempts
- roll back partially admitted sets on any error
- validate unknown outpoints eagerly
- add focused manager tests for:
  - successful spend selection
  - insufficient funds
  - double-selection exclusion
  - spend release
  - spend completion
  - cooperative reserve rejection when spending
  - spend reserve rejection when pending forfeit
  - rollback on partial failure

Reviewer should be able to say:

    "the manager coordinates admission, but actor state owns the lock"

### Commit 3: Wire wallet to the manager admission APIs

This commit should:

- add wallet forwarding handlers for spend select/release/complete
- add wallet forwarding handlers for cooperative reserve/release
- use a small helper to reduce manager Ask/Await boilerplate
- update wallet tests

Reviewer should be able to say:

    "wallet starts operations only after manager-mediated actor admission"

### Commit 4: Move OOR completion to actor-owned finalization

This commit should:

- stop writing `VTXOStatusSpent` directly in the OOR local persistence path
- route OOR spend completion through manager -> VTXO actor
- persist `Spent` from the VTXO outbox path
- update OOR tests and any handler wiring needed for durable completion

Reviewer should be able to say:

    "OOR completion no longer bypasses the VTXO actor"

### Commit 5: Gate cooperative round registration with prior admission

This commit should:

- reserve refresh/leave inputs through the manager before `RegisterIntentMsg`
- release them if round registration fails
- remove round-side `PendingForfeitEvent` notification as the primary
  admission mechanism
- keep `PendingForfeitEvent` rejection in `SpendingState` as an invariant
  guard, not the normal control path
- add tests covering both race directions

Reviewer should be able to say:

    "cooperative and OOR starts exclude one another before round intent
    registration"

### Commit 6: Recovery, docs, and cleanup

This commit should:

- update recovery tests for persisted `SpendingState`
- update docs and comments that still describe direct OOR spent writes or
  round-owned pending marking
- update the overarching roadmap/progress notes

Reviewer should be able to say:

    "implementation, recovery model, and docs all match"

## Files Likely Touched

- `vtxo/states.go`
- `vtxo/events.go`
- `vtxo/interfaces.go`
- `vtxo/transitions.go`
- `vtxo/transitions_test.go`
- `vtxo/actor.go`
- `vtxo/messages.go`
- `vtxo/manager.go`
- `vtxo/manager_test.go`
- `wallet/wallet.go`
- `wallet/wallet_test.go`
- `round/actor.go`
- `round/actor_test.go`
- `oor/local_persistence_handler.go`
- `oor/transitions.go`
- `oor/actor_test.go`
- `oor/actor_resume_test.go`
- `darepod/rpc_server.go`

## Acceptance

After PR 2:

- two concurrent spend selections never receive the same VTXO
- a VTXO in `SpendingState` cannot be admitted for cooperative consumption
- a VTXO in `PendingForfeitState` or `ForfeitingState` cannot be admitted for
  spending
- cooperative admission happens before round registration, not after
- OOR completion transitions VTXOs to `Spent` through the VTXO actor path
- every failure path releases partially admitted VTXOs
- `SpendingState` is recovered after restart and is not silently cleared
- expiry safety still works while a VTXO is in `SpendingState`
- existing focused tests still pass with updated expectations
