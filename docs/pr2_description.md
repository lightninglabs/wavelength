## Summary

Replaces #168 and closes #150.

This PR implements VTXO admission as an actor-owned state machine rather
than a manager-owned in-memory lock set.

The key change is that VTXOs now know when they are claimed:

- OOR spend admission moves a VTXO into `SpendingState`
- Cooperative refresh/leave admission moves a VTXO into
  `PendingForfeitState` before round registration
- OOR completion is finalized through `manager -> VTXO actor`, so the
  VTXO itself transitions to terminal `SpentState`

This preserves a single source of truth for coin availability and closes
the race that blocked #168: an OOR spend and a cooperative flow could
previously compete for the same VTXO if the VTXO actor itself did not
know it was already claimed.

## Why This Replaces #168

#168 introduced manager-side locking to prevent double-spends during coin
selection. The core review feedback there was correct: hidden manager
locks were not sufficient, because cooperative flows still ran through
`wallet -> round <-> VTXO` and could bypass that admission model.

This PR replaces that approach with actor-owned admission:

- the manager is still the coordinator
- the VTXO FSM is now the authority
- conflicting operations fail against actor state, not against a side
  lock table

That means this PR solves the same problem as #168, but in a way that
also covers refresh/leave and recovery correctly.

## New Model

```text
                admission request
wallet / OOR / round-facing flow
          |
          v
    +-------------+
    | VTXO Manager|
    | coordinator |
    +-------------+
          |
          | Ask actor to claim state
          v
    +-------------+
    | VTXO Actor  |
    | source of   |
    | truth       |
    +-------------+
          |
          +--> Live
          +--> Spending
          +--> PendingForfeit
          +--> Forfeiting
          +--> Spent / Forfeited / Expiring / Failed
```

## State Model

```text
OOR spend:
  Live
    -> Spending
    -> Spent

Cooperative refresh / leave:
  Live
    -> PendingForfeit
    -> Forfeiting
    -> Forfeited

Expiry escalation:
  Live / Spending / PendingForfeit / Forfeiting
    -> UnilateralExit
```

## OOR Admission Flow

```text
RPC / caller
  -> wallet.SelectAndLockVTXOs
  -> VTXO manager SelectAndReserveSpendRequest
  -> manager selects live VTXOs
  -> manager Ask SpendReserveEvent on each VTXO actor
  -> VTXOs transition Live -> Spending
  -> wallet receives selected set
  -> OOR flow proceeds

On cancel/failure:
  -> wallet.UnlockVTXOs
  -> manager ReleaseSpendRequest
  -> VTXOs transition Spending -> Live

On success:
  -> OOR persistence emits MarkInputsSpent
  -> LocalPersistenceOutboxHandler calls manager CompleteSpendRequest
  -> manager Ask SpendCompletedEvent on each VTXO actor
  -> VTXOs transition Spending -> Spent
```

## Cooperative Admission Flow

```text
wallet.RefreshVTXOs / LeaveVTXOs
  -> build intent package locally
  -> manager ReserveForfeitRequest
  -> manager Ask PendingForfeitEvent on each VTXO actor
  -> VTXOs transition Live -> PendingForfeit
  -> wallet sends RegisterIntentMsg to round

If round rejects registration:
  -> wallet ReleaseForfeitRequest
  -> VTXOs transition PendingForfeit -> Live

If round accepts:
  -> normal cooperative lifecycle proceeds
  -> PendingForfeit -> Forfeiting -> Forfeited
```

## Recovery Model

Persisted non-terminal VTXO state is now meaningful across restart.

```text
DB status on restart
  -> manager recovers VTXO actors
  -> actor restores FSM state

Live            -> LiveState
Spending        -> SpendingState
PendingForfeit  -> PendingForfeitState
Forfeiting      -> ForfeitingState
Spent           -> SpentState
```

This avoids silently dropping spend/cooperative claims on restart.

## Main Changes

- Add `SpendingState` and `SpentState` to the VTXO FSM
- Add persisted `VTXOStatusSpending`
- Add manager admission APIs for:
  - select/reserve spend
  - release spend
  - complete spend
  - reserve forfeit
  - release forfeit
- Move shared admission messages to `lib/actormsg`
- Register and start the VTXO manager in daemon startup
- Route wallet admission calls through the manager service key
- Gate cooperative round registration on prior admission
- Route OOR spend completion through manager -> VTXO actor
- Add RPC exposure for `VTXO_STATUS_SPENDING`
- Add manager, wallet, and recovery tests covering:
  - spend/cooperative conflict exclusion
  - rollback on failed admission
  - duplicate outpoint normalization
  - invalid target rejection
  - recovery of persisted `Spending` / `PendingForfeit`

## Reviewer Notes From #168

This PR incorporates the review feedback from #168 that still applied:

- VTXOs now know when they are claimed
- cooperative flows are gated before round registration
- manager only operates on known tracked VTXOs
- wallet manager-call boilerplate is reduced via a helper
- focused admission and recovery tests were added

## Test Plan

- `go test ./darepod ./oor ./wallet ./vtxo ./round`
