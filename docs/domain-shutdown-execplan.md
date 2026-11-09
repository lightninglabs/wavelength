# Join domain shutdown before releasing shared stores

This ExecPlan follows `PLANS.md`. Keep its implementation and validation state
current. It describes a source change only; it authorizes no deployment.

## Purpose

Stopping an embedded wallet must not close its database while child actors or
detached writes still use it. A caller deadline ends that caller's wait. The
existing SDK cleanup owner remains alive until daemon work actually returns.

## Progress

- [x] Trace native child, detached writer and RPC cleanup ownership.
- [x] Keep OOR/unroll/credit child counters beyond active-map removal.
- [x] Stop OOR/unroll FSMs only after their native mailbox turns have joined.
- [x] Join unroll persistence, drain probes and abandoned-broadcast cleanup.
- [x] Join the fraud watcher and detached submitted-transfer RPC cleanup.
- [x] Remove the daemon's internal timeout that permitted database teardown
  after unsuccessful native joins.
- [x] Validate all five affected packages and run ownership negative controls.

## Design and decisions

The registry receive loop is the child admission owner. Shutdown first joins
that loop, then waits for child workers. A native child's count remains live
through its last access. Detached work increments the same group while its
producer still owns a live turn. Its final defer releases the count.

A terminal active-map entry is only a routing handle. Removing it cannot release
lifecycle ownership. No second map of retired children is needed. OOR's legacy
bounded Stop API remains available; daemon teardown uses its strong joined API.

Read/Commit native actors do not invoke behavior OnStop automatically. Wrapper
stop cancels the native actor and owns a joined cleanup continuation. The
continuation waits for the native turn before touching its mutable FSM.
Stable-ID chain watch registration keeps its pre-existing lifecycle. This
change does not introduce a late unregister that could remove a replacement
child's watches.

The daemon waits without a UI deadline before closing shared resources. The SDK
still bounds each caller's wait. No caller may treat a timeout as completed
shutdown. Detached OOR cleanup preserves its existing ambiguity deadline and
input-release decisions; this change tracks it without accelerating release.

## Validation

Run formatting and changed-source lint against the integration prerequisite
branch. Run `go test -race ./oor ./unroll ./credit ./fraud ./waved` plus focused
ownership cases. The unroll case blocks a real asynchronous registry write and
proves Stop waits for its final access. The OOR case keeps a real native child's
mailbox-store read alive across cancellation, without an active-map handle, and
proves registry join retains it. Negative overlays remove the unroll join and
OOR child lifetime assignment respectively; both assertions must fail.

Validate package documentation mirrors and links. No migration, wire protocol,
wallet signing policy or remote service change is required.

## Limits and next work

This change does not implement a host execution window, normal missed-event
catch-up or canonical receipt projection. It does not assert universal safe
suspension for optional HTTP diagnostics or every future subsystem. Host mode
must explicitly disable or separately own unsupported services. Slow external
cleanup may exceed a caller's budget; retaining ownership is the required
outcome, rather than releasing a live store.

## Recovery

No persistent format changes. A source rollback retains the earlier shutdown
behavior and must not be advertised as a joined embedded-host boundary. Do not
activate a dependent bounded-host integration before its prerequisites merge.

Detached outgoing-transfer RPC cleanup keeps its existing ten-minute ceiling
(`submittedOORCleanupTimeout`). Daemon shutdown joins that cleanup before the
wallet actor stops. A slow or unsettled future can therefore retain shutdown
until that bound; this PR does not promise a hard background completion time.
Detached unroll admission watchers also observe registry shutdown, so an
unclaimed Ask future cannot retain their join indefinitely.
