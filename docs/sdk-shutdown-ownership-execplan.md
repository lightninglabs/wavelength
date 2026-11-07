# Retain SDK cleanup across caller timeouts


This ExecPlan follows `PLANS.md`. Keep its Progress, Surprises & Discoveries,
Decision Log, and Outcomes & Retrospective current.

## Purpose / Big Picture


A phone host can stop waiting for shutdown without losing the only handle to a
running wallet. Repeated Stop or CloseContext calls wait the same cleanup. A new
mobile Start remains rejected until that cleanup returns. This prevents two
instances from opening one wallet directory after a slow shutdown. It does not
promise a globally suspend-safe daemon state or implement a background window.

## Progress


- [x] (2026-11-07) Identify cached-timeout and mobile owner-drop paths.
- [x] (2026-11-07) Implement one handle-owned cleanup, repeatable caller waits,
  direct callback admission/cancellation/join and retained mobile generations.
- [x] (2026-11-07) Run focused callback and startup regressions under the race
  detector, then full tagged Go SDK and mobile race suites.
- [x] (2026-11-07) Complete default and mobile-tagged lint, actor blocking
  checks, formatting and documentation checks.

## Surprises & Discoveries


The private gRPC server does not own direct signer and recovery callbacks. A
caller can invoke those while Close starts resource teardown. They need one
handle-owned admission boundary and final-access join. Cancelling their contexts
alone does not prove that they have finished.

Startup errors also discard unpublished handles. Keeping ownership only after a
successful Start would leave the failed startup and external-seed rollback paths
unsafe. Those paths now join their daemon before returning the error.

## Decision Log


On 2026-11-07, keep the existing five-second Stop/Close wait and add CloseContext
for a caller-selected wait. The handle starts one cleanup goroutine whose context
is independent of the first waiter. A timeout is a waiter result, not the cached
cleanup result. Do not force teardown after a caller deadline.

On 2026-11-07, put all four direct callbacks behind the same mutex and wait group:
operator signing, swap signing, embedded readiness and recovery. Admission and
shutdown share the mutex, so no wait-group addition can race the cleanup wait.
Merge caller cancellation with handle cancellation without inheriting the first
request's lifetime as the handle root.

On 2026-11-07, retain the mobile starting/stopping generation until its actual
owner returns. Keep RPC admission closed while stopping. A false IsRunning is
not proof of cleanup completion; callers must wait Stop before restarting.

## Outcomes & Retrospective


Focused and full tagged SDK/mobile race suites pass. Default and mobile-tagged lint, actor blocking checks, formatting and
documentation pass. 
Daemon domain joins, durable wake recovery and amount presentation remain
separate work. No deployment, Apple setup, or framework publication occurs here.

## Context and Orientation


`Client` in `sdk/wavewalletdk/client.go` owns its close function and direct
callbacks. `sdk/wavewalletdk/embedded.go` runs an embedded waved daemon and
creates its private gRPC connection. `sdk/wavewalletdk/external_seed.go` owns
opening and rollback before publishing a client. The singleton in
`sdk/wavewalletdk/mobile/mobile.go` exposes those owners through gomobile-safe
free functions, since that ABI cannot carry Go context values.

A join means waiting until an owner's last resource access has returned.
Cancellation requests that return; it is not a join. The daemon's Run returning
is the SDK ownership boundary. Some daemon subsystems still need separate work
before Run returning can imply a globally suspend-safe state.

## Plan of Work


Replace cached bounded cleanup in client.go with CloseContext, closeDone and a
single cleanup owner. Add ErrClientClosed and beginLocalCall to serialize direct
callback admission with shutdown. Gate the two notification sources in
notification.go and readiness/recovery in external_seed.go. Retain the existing
caller context values and cancel an admitted call on either caller or handle
cancellation. Join active callbacks before calling the daemon close function.

Remove bounded owner abandonment from embedded.go startup failures and external
seed rollback. In mobile.go, retain a per-generation completion result, keep the
client until cleanup returns, and let repeated Stop callers re-wait it. A Stop
racing startup cancels startup and waits its original owner. Update the SDK and
mobile documentation pairs and existing host lifecycle guide.

## Concrete Steps


From this repository root:

    make fmt-changed
    make lint-changed-local
    make doc-check
    go test -race -tags='swapruntime wavewalletrpc mobile' ./sdk/wavewalletdk ./sdk/wavewalletdk/mobile -count=1 -timeout=5m

Run lint serially with other worktrees because the linter has a global process
lock. This suite uses local fixtures and does not enroll phones or contact APNs.
Expect two successful package lines. Fix failing checks before a signed commit.

## Validation and Acceptance


TestCloseContextRetainsCleanup cancels a waiter while cleanup is blocked, proves
the cleanup context survives, then re-waits the real terminal error exactly once.
TestCloseJoinsLocalCallbacks enters each production direct-callback call site,
observes shutdown cancellation, blocks final access, and proves cleanup cannot
start until that access is released. New calls must return ErrClientClosed.

TestStopRetainsStartingGeneration cancels startup but blocks its return. Stop
must return its caller cancellation, another Start must reject, and a second
Stop must still wait. Release startup, wait Stop successfully, then prove a new
Start and Stop succeed. Existing external-seed rollback and lifecycle tests must
continue passing, including actual SDK and mobile race suites.

## Idempotence and Recovery


Every Close call starts or waits the same cleanup. A host timeout requires a
later wait, not a new daemon or database reset. Startup failure retains the
unpublished daemon until it returns. All changes are source-only and additive to
the public Go API. Retain a final cleanup error for subsequent Go waiters. Mobile
releases the generation only after its owner has returned, even if the result is
an error that the host must handle.

## Artifacts and Notes


Full tagged race evidence:

    ok github.com/lightninglabs/wavelength/sdk/wavewalletdk 38.726s
    ok github.com/lightninglabs/wavelength/sdk/wavewalletdk/mobile 2.230s

## Interfaces and Dependencies


CloseContext(context.Context) error is the additive public API. Stop and Close
remain aliases with their prior five-second wait. beginLocalCall returns a
merged context, release function and error; callers defer release through their
last embedded resource access. The implementation uses standard-library mutex,
wait group, sync.Once and context.AfterFunc, without a new package dependency.

Revision 2026-11-07: document the implemented callback boundary, failed-startup
ownership and verified mobile startup race. Global daemon quiescence is excluded
because it requires its own concrete joins and recovery tests.
