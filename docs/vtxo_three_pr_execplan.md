# Three-PR Plan for VTXO Refactor, Reservation Safety, and In-Round Sends

This ExecPlan is a living document. The sections `Progress`,
`Surprises & Discoveries`, `Decision Log`, and
`Outcomes & Retrospective` must be kept up to date as work proceeds.

This document must be maintained in accordance with [PLANS.md](/Users/elle/LL/darepo-vtxo-coin-select/client/PLANS.md).

## Purpose / Big Picture

This plan splits the current work into three pull requests that each do one
coherent thing.

After PR 1, the VTXO actor finite-state machine (FSM) will have a cleaner
model: it will stop knowing about wallet/round business intents such as
"leave" and "refresh", and it will focus only on coin lifecycle and forfeit
execution. After PR 2, the system will have one clear admission gate that
prevents a VTXO from starting two conflicting operations at once. After PR 3,
the new in-round send feature will be built on top of that safety model rather
than inventing a second, special-case path.

The key goal is to avoid mixing architecture cleanup, concurrency safety, and
feature delivery in one large change. Each pull request should be independently
reviewable, testable, and restart-safe.

## Progress

- [x] (2026-03-11 10:30Z) Captured the three-PR structure in one self-contained
  roadmap.
- [x] (2026-03-11 10:30Z) Recorded the recommended boundary between the VTXO
  FSM refactor and the later reservation work.
- [x] (2026-03-11) PR 1 Phase A: VTXO FSM and routing refactor (7 commits on
  `vtxo-fsm-refactor` branch). Renames, intent removal, routing consolidation,
  expiry liveness, cleanup.
- [x] (2026-03-11) PR 1 Phase B: Simplify round intent registration. Move
  intent composition from round actor to wallet. Add RegisterIntentRequest.
  Remove trigger messages and obsolete round handlers.
- [ ] Implement PR 2: manager-owned reservations and coin selection on top of
  the refactored model.
- [ ] Implement PR 3: in-round send on top of the reservation model.

## Surprises & Discoveries

- Observation: the repository is already split across two mental models. The
  VTXO actor owns refresh/forfeit lifecycle, but OOR spending bypasses the
  actor and directly updates the database status to `Spent`.
  Evidence: `vtxo/states.go` has no `SpentState`, while
  `oor/local_persistence_handler.go` marks inputs spent directly.

- Observation: the current manager lock set only solves one narrow race:
  concurrent spend selection. It does not protect the refresh, leave, or
  expiry-triggered forfeit paths.
  Evidence: spend selection goes through the manager, but the wallet and round
  paths for refresh and leave do not ask the manager whether the coin is
  already reserved.

- Observation: the current `RefreshRequestedState` is semantically closer to
  "pending cooperative forfeit" than to "refresh" as a product concept.
  Evidence: the state is reused for leave already, and the actor ultimately
  waits for `ForfeitRequestEvent`, not for a refresh-specific event.

- Observation: review comments on PR #168 surfaced two concerns that remain
  valid for the redo: tests must cover the new manager methods, and the design
  must answer whether VTXOs themselves need to know that they are "locked".
  Evidence: PR #168 review comment `discussion_r2915575678` asked for test
  coverage of the new manager methods and questioned whether the VTXOs
  themselves need lock awareness.

- Observation: review comments on PR #168 also highlighted the actual race we
  need PR 2 to solve: OOR spending can otherwise race with refresh or another
  cooperative-forfeit attempt.
  Evidence: PR #168 review comment `discussion_r2915870865` explicitly notes
  that if only the manager knows about spend locks, the refresh path can still
  race through `wallet -> round <-> vtxo`.

- Observation: review comments on PR #169 surfaced a family of failure-handling
  requirements for the future in-round send work: unlock or reservation release
  failures must be visible to the caller, partial multi-VTXO trigger failures
  must be handled explicitly, and the feature needs stronger end-to-end testing
  than a manager+wallet integration test alone.
  Evidence: PR #169 comments `discussion_r2912926825`,
  `discussion_r2912926838`, `discussion_r2915926681`, and
  `discussion_r2915915623` all raise concerns about unlock visibility, partial
  trigger failure handling, and whether full send-flow tests belong in a more
  realistic system test layer.

- Observation: the round actor is doing two jobs — round coordination and
  wallet intent composition. `handleTriggerVTXORefresh` and
  `handleTriggerVTXOLeave` load VTXO descriptors from the store, build
  request structs, convert them into IntentPackages, feed the FSM, and notify
  VTXO actors. That composition work belongs in the wallet.
  Evidence: `round/actor.go` lines 1855-1980, where the round actor loads
  VTXOs via `a.cfg.VTXOStore.GetVTXO()` just to build intents.

- Observation: after the Phase A refactor, `TriggerRefreshEvent` and
  `TriggerLeaveEvent` are no longer referenced by any code path. The round
  actor already sends `PendingForfeitEvent` directly. The type definitions
  in `round/vtxo_messages.go` and aliases in `vtxo/events.go` were removed
  during the refactor.

## Decision Log

- Decision: use three pull requests rather than one or two.
  Rationale: the architecture cleanup must land before the concurrency policy,
  and the concurrency policy must land before in-round send so the new feature
  does not build on a transitional model.
  Date/Author: 2026-03-11 / Codex

- Decision: PR 1 will not introduce a generic `Locked` FSM state.
  Rationale: "locked" is too broad. It conflates "reserved for spend",
  "awaiting forfeit", and "must go on-chain". The actor should expose
  operation-specific lifecycle, not one overloaded lock word.
  Date/Author: 2026-03-11 / Codex

- Decision: PR 1 should include routing consolidation so the VTXO actor no
  longer holds direct `RoundActor` or `ChainResolver` outbound refs.
  Rationale: if the VTXO actor can still talk directly to the round actor after
  PR 1, then PR 2's "manager is the admission gate" rule is harder to enforce.
  Routing through the manager makes that later invariant much simpler.
  Date/Author: 2026-03-11 / Codex

- Decision: PR 2 will make the manager the authority for operation admission.
  Rationale: one component must answer "can this VTXO start a new operation
  right now?" If PR 2 does not centralize that check, PR 3 will inherit the
  same concurrency bug under a new feature name.
  Date/Author: 2026-03-11 / Codex

- Decision: PR 2 must validate reservations against actual known VTXOs and not
  silently reserve arbitrary outpoints.
  Rationale: one of the PR #168 review comments correctly pointed out that the
  manager already knows the set of tracked VTXO outpoints. Reservation logic
  should not accept nonsense outpoints and pretend they are protected.
  Date/Author: 2026-03-11 / Codex

- Decision: boilerplate actor Ask/Await/Unpack/type-assert code should be
  reduced when touching those call sites again.
  Rationale: PR #168 review comments noted the repeated actor call boilerplate
  in wallet manager calls. The redo should avoid expanding that pattern further
  if a local helper can keep the code smaller and more consistent.
  Date/Author: 2026-03-11 / Codex

- Decision: PR 3 should reuse PR 2's actor-owned admission model instead of
  inventing a special in-round-send-only lock path.
  Rationale: the user-visible feature is new, but the safety requirement is
  not. Reusing the same admission rules keeps the system coherent, and keeps
  the VTXO actor as the source of truth for coin availability.
  Date/Author: 2026-03-11 / Codex

- Decision: PR 3 must treat reservation-release failures as user-visible
  errors, not warning-only logs.
  Rationale: PR #169 review correctly pointed out that if a send fails and the
  cleanup path also fails, the user needs to be told their funds may remain
  temporarily unavailable.
  Date/Author: 2026-03-11 / Codex

- Decision: PR 3 should explicitly define partial-failure behavior when a send
  touches multiple VTXOs and not all per-VTXO actor triggers succeed.
  Rationale: PR #169 review questioned what happens if some but not all VTXO
  actors accept the in-round send trigger. The implementation plan should not
  leave that as an implicit best-effort path.
  Date/Author: 2026-03-11 / Codex

- Decision: ForfeitRequest should not carry a destination output (DestOutput).
  Rationale: that would reintroduce business intent into the VTXO path. The
  VTXO only says "this coin should be cooperatively consumed." The round/wallet
  layer decides what that consumption means: pair with VTXORequest for refresh,
  pair with LeaveRequest for offboarding, etc.
  Date/Author: 2026-03-11 / elle

- Decision: the round actor should not compose intents. Add a single
  RegisterIntentRequest entry point that accepts a pre-built IntentPackage.
  The wallet loads descriptors, chooses VTXOs, builds the package. The round
  validates and registers it, then notifies affected VTXO actors.
  Rationale: removes TriggerVTXORefreshMsg/TriggerVTXOLeaveMsg indirection,
  removes VTXOStore dependency from round config, and sets up cleanly for
  coin selection in PR 2 where the wallet picks VTXOs before building intents.
  Date/Author: 2026-03-11 / elle

- Decision: derive ForfeitOutpoints from IntentPackage.Intents.Forfeits rather
  than passing them separately in RegisterIntentRequest. Only notify VTXO
  actors (PendingForfeitEvent) after round FSM registration succeeds.
  Rationale: avoids redundant data, avoids marking VTXOs pending for a round
  that rejected them.
  Date/Author: 2026-03-11 / elle

- Decision: keep RefreshVTXORequest as a short-term compatibility wrapper for
  the auto-expiry path. The VTXO actor's expiry-driven ForfeitRequest still
  needs to reach the round actor. The shim converts it into an IntentPackage
  internally. Remove it later when expiry becomes warning/policy-driven from
  wallet/manager.
  Rationale: keeps temporary behavior local to the existing compatibility
  path. Avoids teaching the manager to assemble round intents, which is the
  wrong long-term responsibility. Manager should be admission/routing/control,
  not another intent composer.
  Date/Author: 2026-03-11 / elle

- Decision: the round actor still needs ActorSystem access to send
  PendingForfeitEvent to VTXO actors after successful intent registration.
  This is acceptable as transitional coupling. Document it as the last place
  where round reaches out to VTXO actors directly.
  Date/Author: 2026-03-11 / elle

## Outcomes & Retrospective

PR 1 Phase A is complete (7 commits on `vtxo-fsm-refactor`). The VTXO actor
no longer knows about leave/refresh business intent. States renamed, routing
consolidated through manager, expiry liveness preserved. Code review found
three issues: leave metadata loss (deferred by design — wallet owns intent
composition), relay error propagation (fixed), stale comments (fixed). Both
fixes squashed into appropriate commits via autosquash rebase.

PR 1 Phase B is complete. Refresh and leave intent composition now live in the
wallet. The round actor accepts RegisterIntentRequest/RegisterIntentMsg,
registers the package with the FSM, and marks participating VTXOs pending.
Trigger messages and obsolete round-side intent composition handlers are gone.

Remaining PRs (2: reservations, 3: in-round send) are unchanged from the
original plan.

## Context and Orientation

A VTXO is an off-chain coin tracked by the client. Each live VTXO has an actor
in `vtxo/actor.go`. That actor runs a finite-state machine, which means it has
one explicit state and changes that state when events arrive. The concrete
state definitions live in `vtxo/states.go`, and the transition logic lives in
`vtxo/transitions.go`.

The VTXO manager in `vtxo/manager.go` owns the set of active VTXO actors and,
in the current branch, also owns transient in-memory locking used by spend
selection. The wallet actor in `wallet/wallet.go` is the user-facing entry
point. The round actor in `round/actor.go` coordinates cooperative batch
operations such as refresh and leave. The OOR (out-of-round) path in
`oor/actor.go` and `oor/local_persistence_handler.go` handles off-round
transfers and currently updates VTXO spend status directly in the database.

Today, those pieces do not line up cleanly:

- refresh and leave are actor/round-driven
- OOR spending is manager/wallet-driven
- `RefreshRequestedState` is really a "pending cooperative forfeit" state
- a manager-side lock can reserve a coin for spend, but the refresh/leave path
  does not consult that lock

This plan fixes those mismatches in stages rather than all at once.

## Overall Design

The system after all three PRs should have these properties:

- The VTXO actor knows only about coin lifecycle, forfeit execution, and
  unilateral exit escalation.
- The manager decides whether a new operation may start on a given VTXO.
- The wallet layer decides *why* a VTXO should be consumed cooperatively
  and composes the concrete intent package: refresh, leave, consolidation,
  or in-round directed send.
- The round actor and OOR actor execute or coordinate the operation but do
  not decide what it contains.
- In-round send reuses the same reservation and availability logic as OOR
  spend selection.

The long-term conceptual stack should look like this:

    wallet / RPC layer
      decides user intent and composes the operation:
        refresh  = Forfeits + VTXOs
        leave    = Forfeits + Leaves
        send     = Forfeits + VTXOs (directed)
        OOR send = separate path via OOR actor

    round actor / OOR actor
      executes or coordinates the composed operation
      (round registers IntentPackage, OOR executes transfer)

    manager
      answers: "may this operation start on this VTXO?"

    VTXO actor
      answers: "what lifecycle phase is this coin in right now?"

## PR 1: VTXO FSM and Routing Refactor

### Goal

Make the VTXO actor a leaf actor that understands coin lifecycle, not wallet
intent. The actor should stop knowing about `leave` and should stop treating
`refresh` as a first-class business concept. Its job is to:

1. monitor expiry
2. signal when cooperative action is needed
3. sign forfeit transactions when asked
4. escalate to unilateral exit when expiry becomes critical

### Target FSM for PR 1

PR 1 should leave the VTXO actor with a state model shaped like this:

    +--------------------+
    |       Live         |
    +--------------------+
      | committed to cooperative consumption, but no concrete
      | forfeit details to sign yet
      v
    +--------------------+
    |  PendingForfeit    |
    +--------------------+
      | concrete ForfeitRequestEvent from round
      v
    +--------------------+
    |    Forfeiting      |
    +--------------------+
      | round confirms new commitment / forfeit completion
      v
    +--------------------+
    |    Forfeited       |
    +--------------------+

    fast path when concrete forfeit details already exist:

    Live --ForfeitRequestEvent--> Forfeiting

    Live / PendingForfeit / Forfeiting
      --critical expiry-->
    +--------------------+
    | UnilateralExit     |
    +--------------------+

    any non-terminal
      --VTXOFailedEvent-->
    +--------------------+
    |      Failed        |
    +--------------------+

Notes:

- `PendingForfeit` is the recommended replacement for the current
  `RefreshRequestedState`.
- `PendingForfeit` means: this VTXO has already been committed to cooperative
  round consumption and is no longer freely available, but the round has not
  yet delivered the concrete `ForfeitRequestEvent` data needed to sign.
- Do not call that non-terminal state `ExpiringState`. The name `Expiring`
  makes it sound like the unilateral path, not the cooperative path.
- `UnilateralExitState` is terminal for this actor because the chain resolver
  takes over after that point.
- The design should allow both:
  `Live -> PendingForfeit -> Forfeiting`
  and
  `Live -> Forfeiting`
  The first is the common asynchronous path. The second is a fast path for
  cases where the round already has concrete forfeit details.

### Scope

PR 1 should:

- rename the current terminal `ExpiringState` to something like
  `UnilateralExitState`
- replace `RefreshRequestedState` with `PendingForfeitState`
- remove VTXO actor concepts that are really wallet/round intent:
  `TriggerRefreshEvent`, `TriggerLeaveEvent`, `RefreshAcknowledgedEvent`,
  `LeaveRequest`
- keep `ForfeitRequestEvent` and `ForfeitConfirmedEvent` as the concrete round
  lifecycle inputs to the actor
- move VTXO outbound signals through the manager rather than directly to both
  round and chain-resolver refs
- remove direct `RoundActor` and `ChainResolver` outbound refs from the VTXO
  actor config by the end of PR 1
- preserve the current liveness guarantee for expiry-driven cooperative action
  by defining a default manager-side policy when a VTXO emits an expiry warning
  or enters `PendingForfeit`

### Important non-goal

PR 1 should not try to solve the spend-vs-forfeit concurrency problem by
itself. It should prepare the code so PR 2 has one clean place to add that
policy.

### Liveness requirement

PR 1 must not regress the existing behavior where approaching expiry
automatically leads to cooperative action being attempted. If the VTXO actor
stops talking directly to the round actor and instead warns the manager, the
manager must still apply a default automatic policy. The simplest default is:

    ExpiryWarning
      -> manager requests cooperative refresh/forfeit automatically
      -> wallet may later override this policy, but safety does not depend on
         wallet reaction time

This keeps the architecture cleaner without making expiry safety depend on a
manual or delayed wallet decision.

### Files likely touched

- `vtxo/states.go`
- `vtxo/transitions.go`
- `vtxo/events.go`
- `vtxo/outbox_messages.go`
- `vtxo/actor.go`
- `vtxo/manager.go`
- `round/actor.go`
- `round/vtxo_messages.go`
- `wallet/wallet.go`
- VTXO and round test files

### Acceptance

After PR 1:

- the VTXO actor no longer knows about leave as a first-class concept
- the non-terminal pre-forfeit state is clearly named as cooperative
  consumption, not refresh-specific intent
- the VTXO actor no longer holds a direct outbound ref to the round actor;
  round-bound signals (ForfeitRequest, ForfeitSignatureSubmission) are routed
  through the manager
- **Deferred:** the VTXO actor still holds a direct ChainResolver ref and
  sends ExpiringNotification directly via the outbox handler. Routing this
  escalation path through the manager is deferred until the chain resolver
  integration is fully wired up (PR 2 or PR 3)
- expiry-driven cooperative action still happens automatically via a manager
  default policy rather than relying on wallet reaction time
- the code path from "coin needs cooperative action" to
  `ForfeitRequestEvent -> Forfeiting -> Forfeited` is simpler than today
- all existing focused tests for `vtxo`, `round`, and `wallet` still pass after
  necessary updates

### Suggested Commit Plan for PR 1

PR 1 should be split into small commits that each move one concept at a time.
The order below is deliberate: the early commits rename and clarify existing
concepts without changing architecture too much, and the later commits move
routing and policy.

#### Commit 1: Rename unilateral-expiry state and status

Purpose:
make the current terminal expiry handoff state read correctly before any deeper
FSM changes happen.

This commit should:

- rename the current terminal `ExpiringState` to `UnilateralExitState`
- rename any matching status names if needed, or introduce aliases if a staged
  migration is easier
- update recovery helpers, stringers, and tests to use the new name

This commit should not:

- introduce `PendingForfeitState`
- change routing
- change wallet or round behavior

Reviewer should be able to say:

    "this is just a naming correction; behavior is unchanged"

#### Commit 2: Replace RefreshRequested with PendingForfeit

Purpose:
make the pre-forfeit lifecycle reflect cooperative consumption rather than the
product concept of "refresh".

This commit should:

- replace `RefreshRequestedState` with `PendingForfeitState`
- update transitions so the state means "committed to cooperative consumption,
  awaiting concrete forfeit details"
- preserve the direct `Live -> Forfeiting` fast path for cases where a real
  `ForfeitRequestEvent` already exists
- update persisted status naming if the repository is ready for that now, or
  add a clear compatibility bridge if the status rename must be staged

This commit should not:

- remove VTXO-round direct routing yet
- remove wallet/round trigger entry points yet
- change manager admission logic

Reviewer should be able to say:

    "the VTXO state model is cleaner, but the architecture is still the same"

#### Commit 3: Remove VTXO business-intent events

Purpose:
stop the VTXO actor from pretending it understands wallet/round business
concepts like leave and manual refresh intent.

This commit should:

- remove or deprecate `TriggerRefreshEvent`, `TriggerLeaveEvent`, and
  `RefreshAcknowledgedEvent` from the VTXO-facing FSM path
- remove `LeaveRequest` and any other VTXO outbox message whose meaning is a
  wallet/round business intent rather than a lifecycle signal
- adjust tests so the actor is driven by lifecycle events rather than product
  intent events

This commit may temporarily keep compatibility glue in round or wallet if that
reduces churn, but the VTXO actor itself should stop branching on leave vs
refresh.

Reviewer should be able to say:

    "the leaf actor now only speaks lifecycle"

#### Commit 4: Route VTXO outbound signals through manager

Purpose:
remove direct outbound `RoundActor` and `ChainResolver` refs from the VTXO
actor so the manager becomes the only outward router for VTXO actor signals.

This commit should:

- route forfeit-signature submission through the manager
- route unilateral-exit escalation through the manager
- remove `RoundActor` and `ChainResolver` refs from `VTXOActorConfig`
- add manager relay handlers needed to preserve behavior

This commit should not:

- add reservation logic yet
- change the meaning of coin selection

Reviewer should be able to say:

    "the VTXO actor now only talks outward through the manager"

#### Commit 5: Add manager-driven expiry liveness policy

Purpose:
preserve safety and liveness after routing consolidation by ensuring that an
expiry warning still triggers automatic cooperative action rather than waiting
for a human or slow wallet reaction.

This commit should:

- define the manager-side handling of expiry warnings
- implement the default automatic policy for cooperative action
- make wallet notification optional or additive, not the sole path required for
  liveness
- add focused tests that prove an expiring coin still advances toward
  cooperative handling without manual intervention

Reviewer should be able to say:

    "the refactor did not make expiry safety depend on wallet reaction time"

#### Commit 6: Remove obsolete round and wallet plumbing

Purpose:
delete the temporary compatibility paths that existed only while the actor was
being refactored.

This commit should:

- simplify `round/actor.go` and `wallet/wallet.go` to match the new PR 1
  architecture
- remove dead messages, helpers, and comments that referred to the pre-refactor
  VTXO behavior
- update integration-style tests that were still using the old trigger model

Reviewer should be able to say:

    "all layers now agree on the new PR 1 architecture"

#### Commit 7: Final test and documentation cleanup

Purpose:
leave PR 1 in a reviewable, documented state with no stale references.

This commit should:

- finish comment cleanup
- move or rename tests if their package placement changed during review
- update docs or README excerpts that still describe the old flow
- run and confirm the focused PR 1 test set

Reviewer should be able to say:

    "the implementation and docs match"

### Phase B: Simplify Round Intent Registration

#### Goal

Move refresh/leave intent composition out of the round actor and into the
wallet. After this change:

- wallet decides what the user wants
- wallet builds the full round intent package
- round only validates/registers the package
- VTXO actor only sees generic lifecycle signals like PendingForfeitEvent

This removes:

- `TriggerVTXORefreshMsg`
- `TriggerVTXOLeaveMsg`
- round-side descriptor loading just to build intents

#### Current Problem

Today the round actor is doing two jobs:

1. round coordination
2. wallet intent composition

That shows up in the refresh/leave paths:

- wallet sends a trigger
- round loads VTXOs from store
- round builds request structs
- round converts them into an IntentPackage
- round feeds the FSM
- round tells VTXO actors PendingForfeitEvent

That composition work belongs in the wallet.

#### Target Boundary

Wallet:

- load VTXO descriptors if needed
- choose which VTXOs are being consumed
- build IntentPackage

Round:

- accept a prebuilt package
- validate/register it with the FSM
- after successful registration, notify affected VTXO actors that they are
  pending cooperative consumption

VTXO actor:

- does not know leave
- does not know refresh
- only knows generic lifecycle events

#### New Round API

Add a single primary entry point:

    type RegisterIntentRequest struct {
        actor.BaseMessage

        // Package is the fully composed round intent bundle.
        Package *IntentPackage
    }

Handler behavior:

1. find or create the pending round FSM
2. feed Package into the FSM
3. if registration succeeds, derive forfeited outpoints from
   `Package.Intents.Forfeits`
4. send `PendingForfeitEvent` to each affected VTXO actor
5. return success/failure to the caller

Important points:

- use Ask, not Tell
- do not pass ForfeitOutpoints separately — derive them from the package
- only notify VTXO actors after round registration succeeds
- if registration succeeds but a PendingForfeitEvent notification fails for
  one VTXO, log the failure and continue notifying the remaining VTXOs. The
  round FSM already has the intent registered, so the forfeit will proceed
  when the round advances. The VTXO that missed the notification will
  receive the concrete ForfeitRequestEvent later and transition directly
  from Live to Forfeiting via the existing fast path. This is safe because
  PendingForfeitEvent is an optimization (marks the VTXO unavailable early),
  not a correctness requirement for the forfeit itself.

#### Wallet Changes

Wallet becomes responsible for building packages.

Refresh path:

- load target VTXOs
- build `ForfeitRequests` for the consumed VTXOs
- build `VTXORequests` for replacement outputs
- send one `RegisterIntentRequest`

Leave path:

- build `ForfeitRequests` for the consumed VTXOs
- build `LeaveRequest` for the destination output
- send one `RegisterIntentRequest`

Conceptually:

- refresh = Forfeits + VTXOs
- leave = Forfeits + Leaves
- later send = Forfeits + VTXOs (or other combinations)

#### Round Actor Cleanup

After callers are switched, remove:

- `TriggerVTXORefreshMsg`
- `TriggerVTXOLeaveMsg`
- `handleTriggerVTXORefresh`
- `handleTriggerVTXOLeave`

Then, if no longer needed:

- `RefreshVTXORequest` (but see auto-expiry bridge below)
- `LeaveVTXORequest`
- round-side helpers that only existed to compose intents
  (`buildRefreshVTXORequest`, `buildVTXORequestFromRefresh`)
- `VTXOStore` from round config, if nothing else uses it

Two phases:

1. add new API and switch callers
2. delete wrappers and old config once the new path is stable

#### Auto-Expiry Bridge

The current auto-expiry path works like this:

    VTXO block epoch → ExpiryStatusNeedsRefresh → ForfeitRequest outbox
      → processOutbox builds RefreshVTXORequest → relay through manager
      → round actor handles RefreshVTXORequest → builds IntentPackage

Keep `RefreshVTXORequest` as a compatibility wrapper. The round actor's
handler converts it into an `IntentPackage` internally. This keeps the
temporary behavior local to the existing path and avoids teaching the
manager to assemble round intents.

Long-term target:

- VTXO actor emits expiry warning
- manager/wallet policy decides to refresh
- wallet builds `RegisterIntentRequest`
- round registers it

Remove the `RefreshVTXORequest` shim once that long-term path is in place.

#### Coin Selection

This structure fits coin selection cleanly:

1. wallet selects/reserves VTXOs (PR 2)
2. wallet builds IntentPackage
3. wallet sends RegisterIntentRequest
4. round registers it
5. VTXOs enter pending cooperative consumption

Round no longer needs special trigger messages for spend/refresh/leave.

#### Suggested Commit Sequence for Phase B

1. add `RegisterIntentRequest` and round-side handler
2. switch wallet refresh flow to build/send full intent package
3. switch wallet leave flow to build/send full intent package
4. remove trigger messages and obsolete round handlers
5. remove now-unused round config/helpers/types
6. test cleanup and coverage

#### Design Rules

- round should register intents, not compose them
- wallet should own business intent composition
- VTXO should only model lifecycle, not user intent
- avoid duplicate sources of truth in request types
- use request/response semantics for registration path

### Commit-level Validation for PR 1

Each commit in PR 1 should build on its own. The minimum expectation after each
commit is:

    go test ./vtxo

After commits 4 through 7, because routing and cross-package wiring are being
changed, the expectation should expand to:

    go test ./vtxo ./round ./wallet

Before opening the PR, run the repository-preferred focused checks for the
affected packages, including formatting and linting, so the review stays about
design rather than mechanical cleanup.

## PR 2: Reservation Safety and Coin Selection

### Goal

Build the coin-selection work on top of PR 1 by making the manager the single
admission gate for all operation starts. This PR is where the actual safety
property is enforced:

    a VTXO that is starting one operation cannot start another conflicting one

### Model

PR 2 should introduce manager-owned reservations with purpose-aware metadata.
Do not use a bare boolean if the purpose can be represented explicitly.

Suggested shape:

    ReservationPurpose:
      spend
      cooperative_forfeit
      sweep

    Reservation:
      outpoint
      reservation_id
      purpose

The key rule should be:

    available = lifecycle_allows_new_operation
                AND no_active_reservation

The current `SelectAndLockVTXOsRequest` shape may stay in PR 2 even if the
internal implementation changes later. In the short term, the message still
means:

    1. wallet asks manager for VTXOs covering a target amount
    2. manager filters out reserved outpoints
    3. manager performs selection
    4. manager records a transient reservation and returns the winners

That API shape should be treated as a bridge, not as the final locking design.
Its purpose in PR 2 is to centralize admission and exclusion, not to commit the
system permanently to an in-memory set.

For PR 2, the explicitly reservable lifecycle state should be conservative:

    reservable:
      Live

    not reservable:
      PendingForfeit
      Forfeiting
      Forfeited
      Spent
      UnilateralExit
      Failed

### Scope

PR 2 should:

- keep or evolve the new atomic manager-side spend selection request
- upgrade the manager’s transient lock set into a reservation mechanism
- ensure all cooperative-forfeit starts consult the manager before the round
  actor proceeds
- ensure OOR spend selection also uses that same manager admission check
- release reservations on failure using a reservation identifier, not only an
  outpoint list
- reject reservation or selection attempts for outpoints the manager does not
  actually track as VTXOs
- include focused tests for every new manager admission method and every
  conflict path in both directions

PR #168 review comments that must be treated as carry-forward requirements for
this redo:

- unlock or release reservations on every error path after spend selection has
  succeeded but the transfer fails to start or complete
- answer the "should the VTXO know it is locked?" concern by the architecture
  itself:
  short term, the manager owns reservations;
  long term, a future `SpendingState` makes the actor lifecycle itself the
  exclusion mechanism
- consider a timeout or other cleanup strategy for abandoned reservations, even
  if the first implementation keeps reservations transient and in-memory
- add direct tests for new manager methods rather than relying only on broader
  integration coverage

This is the PR that actually solves the race you described:

    if spend reserves a VTXO first,
      cooperative forfeit start fails

    if cooperative forfeit reserves a VTXO first,
      spend selection skips or rejects that VTXO

### Important architectural point

PR 2 should not let the wallet or round actor start a cooperative forfeit
without going through the manager. If that path bypasses the manager, then the
system still has two sources of truth for availability.

### Forward compatibility with a future SpendingState

PR 2 should be written so that a later `SpendingState` can replace the
transient reservation set without changing the caller-facing flow too much.
When that later work happens, the intended evolution is:

    current PR 2 behavior:
      SelectAndLockVTXOsRequest
        -> manager reads reservable VTXOs from persisted state
        -> manager filters out transient reservations
        -> manager selects winners
        -> manager stores reservations
        -> wallet uses returned VTXOs

    later SpendingState behavior:
      SelectAndLockVTXOsRequest
        -> manager reads reservable VTXOs from persisted state
        -> manager selects winners
        -> manager asks each selected VTXO actor to enter SpendingState
        -> actors that accept are now exclusively claimed
        -> wallet uses returned VTXOs

The key subtlety is that the future system should not merely "filter by actor
FSM state". Selection should still begin from persisted lifecycle state that is
known to be reservable, then the manager should claim the coin by an explicit
actor transition into `SpendingState`. That is safer for crash recovery and
race handling than relying only on an in-memory view of actor state.

### Files likely touched

- `lib/actormsg/interfaces.go`
- `lib/actormsg/service_keys.go`
- `vtxo/manager.go`
- `wallet/wallet.go`
- `round/actor.go`
- possibly `db/sqlc/queries/vtxo.sql` if a narrower availability query is
  needed
- unit and integration tests for `vtxo`, `wallet`, `round`, and
  `internal/coinselect`

### Acceptance

After PR 2:

- two concurrent spend selections never receive the same VTXO
- a VTXO reserved for spend cannot enter cooperative forfeit
- a VTXO already in `PendingForfeit` or `Forfeiting` cannot be selected for
  spend
- reservation cleanup is correct on failure and does not accidentally release
  another workflow’s reservation

## PR 3: In-Round Directed Send

### Goal

Add in-round directed send on top of PR 2's actor-owned admission model. This
PR should replace PR #169 and close issue #156 without reviving the older
send-specific trigger path or introducing a second locking system.

This PR should mostly be feature plumbing and round orchestration, not another
concurrency design exercise.

### Scope

PR 3 should:

- add the in-round send RPC and wallet/round wiring needed to close issue
  #156 and replace PR #169
- reuse the manager's admission rules from PR 2, with the VTXO actor FSM as
  the source of truth
- add an atomic cooperative select-and-reserve path for directed send rather
  than doing "select first, reserve later"; this should likely be a new
  manager API such as `SelectAndReserveForfeitRequest`
- treat "consume this VTXO cooperatively in a round for a directed send" as a
  wallet/round intent, not a VTXO actor business concept
- drive the VTXO actor into `PendingForfeit` and then `Forfeiting` using the
  same cooperative lifecycle already established in PR 1
- keep the wallet as the intent composer and the round actor as the intent
  registrar; avoid reintroducing round-side intent composition
- prefer extending `RegisterIntentMsg` / `IntentPackage` over adding a new
  send-only round trigger unless a concrete data-modeling need forces it
- make change handling explicit and deterministic rather than an implicit side
  effect hidden in one layer
- define dry-run semantics explicitly. Dry-run should exercise the same atomic
  cooperative admission path and then release it immediately, rather than
  degenerating into a weaker balance-only check
- validate recipient and change amounts against operator policy constraints such
  as dust or minimum-output rules before attempting the round
- define cleanup semantics for all failure exits, including what is returned to
  the caller when reservation release itself fails
- define all-or-nothing behavior when multiple selected VTXOs are claimed for
  directed send and one of them cannot be reserved or later registered

More concretely, the target flow should be:

    SendVTXO RPC
      -> validate recipients, total amount, and dry-run rules
      -> wallet asks manager for atomic cooperative select+reserve
      -> selected VTXOs enter PendingForfeitState
      -> wallet builds IntentPackage:
           forfeits + recipient outputs + explicit change output
      -> wallet sends RegisterIntentMsg to round
      -> if round rejects:
           wallet releases cooperative reservation
           release failure is surfaced to caller
      -> if round accepts:
           normal cooperative lifecycle proceeds
           PendingForfeit -> Forfeiting -> Forfeited

For dry-run, the same admission path should be used:

    SendVTXO RPC (dry-run)
      -> validate recipients, total amount, and policy rules
      -> wallet asks manager for atomic cooperative select+reserve
      -> wallet immediately releases the cooperative reservation
      -> if release fails:
           caller receives an explicit warning/error that funds may remain
           temporarily unavailable
      -> return preview response

### Rule

PR 3 must not introduce its own private coin lock. If the feature needs to
reserve inputs, it must do so through the manager path introduced in PR 2.

PR 3 must not treat directed send as an OOR spend. In-round directed send is a
cooperative consume-and-reissue operation, so selected inputs should be driven
through `PendingForfeit`, not `Spending`.

PR 3 must not implement directed send as a split
"select inputs, then call `ReserveForfeit`" flow. Admission must remain atomic
for directed send the same way it is atomic for OOR spend in PR 2.

PR 3 should also avoid a "best effort then log a warning" cleanup story. If the
send fails and the system cannot release the reservation, that must be surfaced
to the caller so the user understands their VTXOs may remain temporarily
unavailable.

### Files likely touched

- `darepod/rpc_server.go`
- `daemonrpc/daemon.proto` and generated RPC files if the send surface changes
- `lib/actormsg/interfaces.go` if `RegisterIntentMsg` or related shared message
  types need to grow
- `round/actor.go`
- `wallet/messages.go`
- `wallet/wallet.go`
- `vtxo/manager.go`
- RPC definitions and tests for the new send flow

### Acceptance

After PR 3:

- in-round directed send works end-to-end
- it cannot select VTXOs already reserved for OOR spend or cooperative forfeit
- it reuses the same availability model as PR 2 instead of creating a second
  locking system
- selected send inputs are claimed through `PendingForfeitState`, not
  `SpendingState`
- the wallet composes the directed-send intent and the round actor registers
  it; there is no return to round-side intent assembly
- if send setup fails and cleanup also fails, the caller receives an explicit
  error that mentions the possible lingering reservation
- partial multi-input admission/registration failures are handled
  deterministically (either rolled back or otherwise made explicit by design,
  not left to happen by accident)
- dry-run uses the same admission path as the real send and surfaces release
  failure rather than silently ignoring it
- change and recipient outputs are validated against operator policy before the
  round is started
- the implementation supersedes PR #169 instead of reviving
  `TriggerVTXOSendMsg` / `TriggerSendForfeitEvent`

## Pull Request Boundaries

The most important thing to preserve is the boundary between the PRs.

PR 1 should answer:

    "is the VTXO actor model clean?"

PR 2 should answer:

    "is operation admission safe?"

PR 3 should answer:

    "does in-round send work on top of that model?"

If a proposed code change does not fit the question for the current PR, it
should move to the later one.

## Concrete Steps

Before implementation begins, use these commands to re-orient:

    cd /Users/elle/LL/darepo-vtxo-coin-select/client
    sed -n '1,220p' vtxo/states.go
    sed -n '1,620p' vtxo/transitions.go
    sed -n '1,260p' vtxo/manager.go
    sed -n '640,830p' wallet/wallet.go
    sed -n '1722,1938p' round/actor.go
    sed -n '120,160p' oor/local_persistence_handler.go

Expected observations after Phase A:

    `vtxo/states.go` has `PendingForfeitState` (renamed from
    `RefreshRequestedState`).
    `vtxo/transitions.go` no longer handles TriggerRefreshEvent or
    TriggerLeaveEvent.
    `round/actor.go` still has `handleTriggerVTXORefresh` and
    `handleTriggerVTXOLeave` which load VTXOs and compose intents - this is
    the work for Phase B.
    `wallet/wallet.go` still forwards trigger messages to round; Phase B moves
    intent composition here.
    `oor/local_persistence_handler.go` still marks spent coins directly in the
    DB (addressed in PR 2).

For PR 1 validation:

    go test ./vtxo ./round ./wallet

For PR 2 validation:

    go test ./wallet ./vtxo ./round ./internal/coinselect

Add tests that directly reflect the prior PR #168 review concerns:

- selecting a VTXO and then failing later must release the reservation
- cooperative forfeit start must fail if the same VTXO is already reserved for
  spend
- spend selection must fail or skip when the same VTXO is already committed to
  cooperative forfeit
- reservation attempts for unknown outpoints must be rejected

For PR 3 validation:

    go test ./...

Add narrower focused tests as each PR evolves so that review does not depend
only on the full test suite.

For PR 3 specifically, add at least one higher-fidelity system test that
exercises more than the wallet-manager-round handoff. The prior PR #169 review
was correct that a pure coin-selection integration test is not enough to prove
the new send flow behaves correctly once a real round is created and processed.

Add focused tests that directly reflect the replacement of PR #169:

- directed send coin selection must atomically reserve for cooperative
  consumption, not reserve after selection
- directed send must fail if a chosen input is already in `SpendingState`
- directed send must fail if a chosen input is already in `PendingForfeitState`
- dry-run must use cooperative admission and release, not a weaker
  balance-only check
- change output creation must be explicit and deterministic
- if round registration fails and release also fails, the caller must receive a
  user-visible error mentioning the lingering reservation
- recipient and change outputs below policy thresholds must be rejected before
  round registration

## Validation and Acceptance

Validation must be behavioral, not just compile-only.

PR 1 is accepted when the VTXO actor no longer encodes wallet/round business
intent such as leave, and the pre-forfeit lifecycle is visibly simpler in
tests and code review.

PR 2 is accepted when tests prove that spend selection and cooperative-forfeit
starts exclude one another in both directions.

PR 3 is accepted when the new in-round send feature works while reusing PR 2's
actor-owned admission rules rather than introducing a separate concurrency
mechanism or reviving PR #169's send-specific trigger path.

## Idempotence and Recovery

This roadmap is safe to revisit and refine. The implementation sequence is
deliberately staged so that each PR can be rolled back independently without
forcing a feature rollback in another area.

Do not partially mix PR 2 into PR 1. If PR 1 grows a partial reservation
system, it becomes much harder to review whether the architecture cleanup is
correct on its own.

Do not partially mix PR 3 into PR 2. If PR 2 begins routing in-round send
traffic before the reservation rules are stable, reviewers will have to reason
about both safety and feature behavior at once.

## Artifacts and Notes

Short version of the roadmap:

    PR 1:
      rename and simplify the VTXO lifecycle
      move business intent out of the leaf actor
      route actor outbound signals through manager

    PR 2:
      make the manager the availability gate
      build coin selection on top of that

    PR 3:
      build in-round send on top of PR 2

Short version of the future evolution of `SelectAndLockVTXOsRequest`:

    now:
      selection + transient reservation set

    later:
      selection + actor transition into SpendingState

    final intent:
      SpendingState replaces the separate lock set as the exclusion mechanism

Short version of the state guidance:

    avoid:
      generic LockedState

    prefer:
      Live
      PendingForfeit
      Forfeiting
      Forfeited
      UnilateralExit
      Failed

    later, if desired:
      Spending
      Spent

## Interfaces and Dependencies

At the end of PR 1, the VTXO actor should primarily consume and emit lifecycle
messages rather than business-intent messages. At the end of PR 2, the manager
should expose messages that make it the authority for admission. Stable names
can change during implementation, but the following concepts should exist by
the end of the full roadmap:

In `vtxo/states.go`:

    type LiveState struct { ... }
    type PendingForfeitState struct { ... }
    type ForfeitingState struct { ... }
    type ForfeitedState struct { ... }
    type UnilateralExitState struct { ... }
    type FailedState struct { ... }

In `lib/actormsg/interfaces.go` by the end of PR 2:

    type ReservationPurpose string
    type TryReserveVTXORequest struct { ... }
    type TryReserveVTXOResponse struct { ... }
    type ReleaseVTXOReservationRequest struct { ... }
    type SelectAndReserveVTXOsRequest struct { ... }
    type SelectAndReserveVTXOsResponse struct { ... }

In `round/actor.go` by the end of PR 1 Phase B:

    type RegisterIntentRequest struct { ... }

    handler:
      1. validate and feed IntentPackage to FSM
      2. derive forfeited outpoints from Package.Intents.Forfeits
      3. send PendingForfeitEvent to each affected VTXO actor

    RefreshVTXORequest kept as compatibility shim for auto-expiry path

By the end of PR 3:

    one clear entry point for cooperative VTXO consumption that:
      1. asks the manager whether the VTXO may start (PR 2)
      2. wallet assembles the round intent
      3. round registers it via RegisterIntentRequest
      4. round drives the VTXO actor through the forfeit lifecycle

Revision note: this plan was written to replace ad hoc discussion with one
clear three-PR roadmap: first the VTXO FSM/routing refactor, then reservation
safety and coin selection, then in-round sends on top of that foundation.
