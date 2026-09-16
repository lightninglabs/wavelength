# Batch participation WIP handoff

This snapshot is unfinished and is not ready to merge. It builds on the
UTC timetable discovery draft, #1333. The follow-up branch is
`codex/batch-participation-wip`.

## Current implementation

The daemon, SDK, and CLI carry explicit service selection, operation identity,
expiry, fee caps, and fallback consent. Round admission and quote handling
retain that authorization and enforce participation deadlines. The client
retains inputs when signing outcome or operator ownership is uncertain.

`round/durable_runtime.go` constructs the native persistent mailbox.
`round/durable_behavior.go` stages a before-image before external work and
commits the snapshot, buffered domain writes, outgoing effects, and delivery
ack through the actor runtime. The daemon registers separate typed-service
and outbox-target names and joins the worker before shutting down dependencies.

The snapshot codecs cover round states, fixed environment parameters, queued
quotes, timer deadlines, commands, and effects. Restart restores the snapshot;
when no native checkpoint exists, it imports legacy relational round rows.
Cold signing states retain ownership and await an authoritative dead-round
report instead of creating replacement signer sessions. Already-created
boarding signatures can be resubmitted without generating new nonce material.

## Remaining work, in order

1. Replace `ServiceOperationStore` with an input-ownership projection of the
   native checkpoint, including in-flight commands. The daemon still supplies
   the custom store, and VTXO startup cleanup still queries it. Do not remove
   it until replacement tests prove uncertain inputs remain reserved.
2. Verify real SQLite atomicity of domain mutations, checkpoint, outbox, and
   message consumption under commit failure. Existing turn tests include
   contract doubles; those do not prove the production SQL transaction path.
3. Exercise the shared outbox publisher through the production construction,
   including crash before and after handoff, duplicate effects, and restart
   with pending messages. Cover all three lost-session states and uncertainty
   while entering the nonce ceremony.
4. Complete connected daemon/SDK/CLI flows and the existing randomized workload
   and P replay coverage. The P boundary model and its Go bridge validate a
   storage abstraction, not the whole migrated actor. Perform final review
   before marking the feature ready.

External wallet and signer RPCs are not transactional. Local expiry or
cancellation cannot establish that exposed signatures are unusable. Preserve
exclusive input ownership until the protocol supplies that evidence.

## Validation at publication

On 2026-09-16, changed-code lint passed. Race tests passed for `round`,
`db/actordelivery`, `db`, `waved`, `wallet`, `vtxo`, `lib/types`,
`lib/treecodec`, `sdk/ark`, and `cmd/wavecli/waveclicommands`. Formatting and
`git diff --check` passed. Structural scanning completed with style warnings.
These results do not establish whole-feature acceptance or CI readiness.

Run the nested protocol-runner tests from the parent module using
`go test github.com/lightninglabs/wavelength/baselib/protofsm`; invoking the
standalone baselib module currently requires dependency-sum repair.

Independent review follows draft publication. Record its findings on the
follow-up PR and carry unresolved findings into the next implementation pass.
