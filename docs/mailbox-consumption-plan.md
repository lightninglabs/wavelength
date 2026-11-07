# Complete durable mailbox consumption and semantic replay safety

This ExecPlan is maintained under `PLANS.md`. It covers client-local guarantees.
Independent witnessed history verification is tracked separately in public
issue #1503. A serving peer can still withhold messages; local persistence does
not prove what that peer never recorded.

## Purpose / Big Picture


A wallet must not acknowledge a value-bearing server event merely because an
in-memory queue accepted it. Acknowledgement permits the peer to delete the
remote envelope. The client must first retain that event in a durable inbox,
and must consume it only with the local state and outgoing effects needed to
recover after a crash. Replaying an operation under a different transport ID,
or after a transport receipt expires, must not duplicate a financial effect.

An envelope is the protobuf message received from the peer. A transport receipt
recognizes a previously admitted delivery occurrence for a bounded period. A
semantic identity names the actual protocol operation, such as an owned output
or a round and its protocol phase. These are separate identities: a peer can
change its claimed delivery occurrence without changing the operation.

## Progress


- [x] (2026-11-02) Confirmed current cursor, receipt and bounded quarantine code.
- [x] (2026-11-02) Confirmed production incoming-output delivery uses the durable
  actor constructed by `vtxo.NewIncomingVTXODurableActor`; old tracking that
  calls this an in-memory route is stale.
- [x] (2026-11-02) Separated witnessed completeness into #1503 by explicit scope
  decision. It must not be presented as solved by a server-signed hash chain.
- [x] (2026-11-02) Extracted the synchronous protocol-runner prototype from the
  existing batch-runtime work without replacing the current threaded runner.
  PR #1504 contains the primitive. Consumer migration and its acceptance tests
  remain separate checkpoints.
- [x] (2026-11-04) Isolated the versioned round-state checkpoint format and
  shared tree codec from the older drafts. Field-complete restoration tests
  retain current schedule, local ownership and asset metadata. This is a
  format prerequisite; production actor migration remains below.
- [ ] Prove production inbox/receipt/quarantine recovery for the incoming-output
  route, including a changed occurrence ID and an expired transport receipt.
- [ ] Migrate the current round actor to native durable ownership while retaining
  all current deadline, wallet handoff, finalization and signature-safety fixes.
- [ ] Prove semantic replay behavior at each state-machine ownership boundary.
- [ ] Run crash/reopen, SQL transaction, real-stack, race and review acceptance;
  publish signed logical PRs and keep this plan current through those changes.

## Surprises & Discoveries


The production incoming-output route is already durable in
`waved/server.go:startIncomingVTXOActor`. Its handler retains retryable events,
persists canonical descriptors and acknowledges through the runtime's short
commit. The round actor is still registered with `actor.RegisterWithSystem`.
Its ordinary inbox is volatile. A successful `TryTell` is not durable evidence.

The existing public draft #1334 contains the full round snapshot/turn
implementation; public draft #1335 contains the shared runner and tree codec.
PR #1504 extracts only the runner from #1335. The round draft's
branch predates current changes. Copying its `round/actor.go` wholesale would
remove current wallet nonblocking handoffs, admission deadlines and staged
finalization. Reuse serialization and runner components selectively; preserve
current behavior and prove it with the existing regressions.

## Decision Log


Decision: durable business ownership, not longer-lived transport receipts, owns
semantic replay safety. Rationale: peer-controlled IDs and a finite receipt
horizon cannot establish semantic uniqueness. Date: 2026-11-02.

Decision: retain the current threaded protocol runner and introduce an additive
synchronous runner. Rationale: a durable actor turn must finish all transitions
before its commit boundary; current independently driven actors must not lose
their receive-turn and deadlock safeguards. Date: 2026-11-02.

Decision: do not consume a durable forwarding queue solely after an in-memory
round `Ask` returns. Rationale: successful processing may still leave only
volatile pre-checkpoint state. A forwarding wrapper alone would move the same
crash window rather than close it. Date: 2026-11-02.

## Outcomes & Retrospective


Investigation and the additive runner prototype are complete. Native round
consumption and semantic replay acceptance are not complete. Each prerequisite
and consumer migration needs its own review and merge checkpoint. Deployment
and consumer acceptance remain separate from merging the additive runner.
No completed consumer guarantee should be inferred from this plan or from a
passing primitive test.

## Context and Orientation


Work in the Wavelength repository. `serverconn/ingress.go` owns pull validation,
dispatch and acknowledgement. `serverconn/event_router.go` adapts envelopes;
`serverconn/dispatch_deferral.go` distinguishes durable delivery from bounded
in-memory delivery. `db/actordelivery/store_impl.go` inserts an inbox row and its
transport receipt in the same database transaction. The cursor checkpoint is
part of the enclosing ingress transaction.

`serverconn/ingress_quarantine.go` retains unadaptable complete envelopes. It
removes evidence only when the current recovery dispatch proves a durable
handoff. Existing bounded count/byte policy must remain fail-closed at capacity.

`round/actor.go` owns a map of protocol state machines, buffered quotes, pending
wallet replies, timers, staged ledger writes and retrying finalizations.
`round/interfaces.go:RoundStore` currently persists the signature-bearing
`InputSigSentState`. Earlier states are volatile. `db/round_store.go` implements
that contract. `waved/server.go` constructs both production consumers and the
shared outgoing-message publisher.

## Plan of Work


First add `baselib/protofsm/inline_machine.go` and its tests. Reuse the existing
`StateMachine.applyEvents` transition engine. Do not change the threaded runner,
its `driverFuture`, receive-turn marker handling, or Tell-only outbox delivery.
The new runner must execute the full internal event chain in the caller's
context and return only after transition processing stops. Failure must return
no partial outgoing effects. The owning actor remains responsible for restoring
its last committed state after a failed or ambiguous commit.

Next prove the already durable incoming-output route with real SQLite and
PostgreSQL storage. Exercise original delivery, remote acknowledgement, inbox
processing, process/database reopen, changed transport identity, receipt expiry
and corrected quarantine recovery. Assert durable descriptor/accounting state,
not logging or the number of harmless transport attempts.

For round migration, retain the current actor as the domain owner but give it
one native durable runtime. Serialize all incoming messages, complete protocol
state, buffered routing data, absolute deadlines and pending local work using
versioned type-length-value records. A before-image and in-flight signing intent
must persist before external wallet/signer work. Perform external work outside
the database writer transaction. Commit the updated snapshot, domain writes,
outgoing effects and inbox consumption in one fenced runtime commit. Reload the
last persisted snapshot after a failed or ambiguous commit before another turn
uses tentative in-memory state.

External signer sessions do not survive a process crash. An interrupted nonce
ceremony must not create replacement signing material for a commitment whose
prior signatures may have escaped. Retain input ownership and reconcile against
authoritative protocol state; local expiry alone cannot establish safety. Any
reuse of the existing draft must include fresh fault-injection proof for the
first interrupted signing turn and late signature delivery.

Finally, trace canonical operation identity at each durable consumer. Terminal
operation state and immutable request fingerprints must reject or harmlessly
absorb replays independently of transport IDs and receipt cleanup. Preserve
fresh identical RPC responses: body equality is not an operation identity.

## Concrete Steps


Run commands from the repository root, except where an explicit package is
shown. Start with:

    go test github.com/lightninglabs/wavelength/baselib/protofsm
    go test -race github.com/lightninglabs/wavelength/baselib/protofsm
    go test ./serverconn ./vtxo ./round ./waved

Before each logical commit run the applicable focused tests, `make fmt-changed`,
`make lint-changed-local`, and the changed Go declaration documentation audit.
Run `make commitmsg-lint range=origin/main..HEAD` before pushing. Generate schema
or RPC artifacts only with the repository generators. Run module tidiness after
a clean commit because the current checker rejects unrelated dirty files too.

Record exact failing negative controls, successful commands and reviewed heads
here as implementation progresses. A test claiming restart must reopen the real
store and reconstruct production routing, not merely reuse an in-memory mock.

## Validation and Acceptance


Acceptance requires an acknowledged envelope to survive a process stop before
consumer execution, a crash during external signing work, and a failed local
commit without losing the obligation or emitting duplicate financial effects.
Quarantine evidence can be retired only with a durable current handoff.

For replay, submit the same operation with a fresh envelope/occurrence ID and
again after the transport receipt horizon. Persisted balance, output ownership,
ledger entries and signatures must remain consistent. Independent operations
with identical response bodies must both complete. Repeat under SQLite and
PostgreSQL, with race tests and the relevant real-stack suites. Negative controls
must fail before each owning fix. Full required CI and exact-head risk-aware
review must pass before the implementation is called complete.

## Idempotence and Recovery


Use isolated worktrees and preserve existing draft branches. New database
formats must be versioned and migration-compatible. Never delete evidence or
reset cursors to make recovery appear successful. Never infer remote absence
from a timeout. Keep unresolved input/signature ownership recoverable.

## Artifacts and Notes


Initial focused runner prototype:

    go test github.com/lightninglabs/wavelength/baselib/protofsm -run TestInline
    ok github.com/lightninglabs/wavelength/baselib/protofsm

## Interfaces and Dependencies


The additive `protofsm.InlineStateMachine[E,O,Env]` exposes `Start`, `Stop`,
`IsRunning`, `AskEvent`, `CurrentState` and `CurrentStateWithContext` for the
current actor-facing interface. It reuses existing transition types and performs
no outgoing-message dispatch. Native round construction must use the existing
`actor.TxBehavior` / `actor.Exec` and shared `actor.DeliveryStore`; do not add a
second custom durable queue or bypass the shared SQL transaction context.

Revision 2026-11-02: created after confirming production consumer wiring and the
explicit distinction between local semantic replay safety and witnessed history.
