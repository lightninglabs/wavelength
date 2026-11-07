# Warm background recovery windows

An embedded mobile host can briefly recover one invoice after an authorized
payment hint. The wallet must already be unlocked, retain its daemon handle,
and use the durable round runtime. The API does not unlock a cold wallet or
prove global wallet synchronization.

## Execution order

Start with `durable_round_runtime: true` in mobile configuration. Keep that
setting independent of notification consent after adoption. Startup retains
normal continuous mailbox ingress. Before entering hint-driven background
mode, serialize against all state-creating wallet calls and call
`SetHostForeground(false)`. This joins the old ingress owner. Swap workers,
durable actors, egress and database ownership stay live.

Enroll only a typed receipt reference returned by a newly created invoice
under current consent. A notification payload is never a source reference.
After matching an authorized payment hint to that enrolled invoice, call
`RunReceiptWindow` with a unique window ID, the typed reference, the consent
creation boundary, and a mobile budget of 1 through 20 seconds.

The window first reads the committed local source and canonical projection.
A missing or terminal source returns immediately. A pending source re-arms
only its existing receive worker or credit actor. Duplicate receive workers
share the normal admission gate. Credit Tell acceptance is not a completion
barrier. The controller polls committed source state after bounded mailbox
pumps until completion, an error, a 32-batch bound, or its deadline.

Only exact wired durable destinations are allowed: native round events,
OOR submit/finalize/incoming events, incoming VTXOs and the two proof-backed
indexer response routes. A live response waiter additionally needs original
send-time provenance for one of the two permitted read methods. Manual,
signing and unknown waiters cannot authorize background dispatch. Sender
and protocol-version validation precede this policy. The whole batch is
checked before any waiter delivery, mux request or durable insertion. A
refused batch stays unacknowledged and returns `needs_foreground`.

The two indexer reads are `ListVTXOsByScripts` and
`ListOORRecipientEventsByScript`. These are explicit names, not a blanket
permission for future indexer methods. OOR durable handoffs can use request
kind; kind alone does not establish volatility. Routes bridged through the
non-transactional mux always require foreground.

A quiet mailbox does not mean settlement. Only a returned receipt with
`state: "complete"` and agreeing committed source/canonical state can supply
an amount. The app must separately validate consent, account, network,
publisher and event correlation and persist presentation deduplication.
Timeout, interruption and unavailable proof permit a generic fallback under
the original payment hint. Ordinary protocol wakes never authorize an alert.

## Round and expiry maintenance

`RunMaintenanceWindow` uses the same mode lease, unique window IDs and
`CancelReceiptWindow` cancellation path. Ordinary operator wakes and scheduled
host background tasks can call it without any enrolled invoice. It never reads
or resumes an invoice source and always returns an empty receipt list.

First it samples the configured chain backend's current tip. A primary-key
keyset query selects at most 64 persisted VTXO identities under a finite
ceiling. The manager resolves existing owners only. The caller drives their
ordinary expiry FSM turns outside the manager receive loop, so refresh relays
can drain. A newly expired state gets the existing second reclaim turn. This
can request cooperative refresh, retain a reservation, persist expiry or begin
a critical exit according to existing policy. It does not rerun startup repair,
open the round-startup gate or spawn actors from historical rows. A delayed
sample below a newer delivered block is ignored by the owning VTXO actor.

The controller retains progress across warm windows. An interrupted owner is
retried; a finished finite sweep starts again next time. New keys behind the
cursor join that next sweep. Process startup retains its ordinary full expiry
reconciliation. Historical rows consume bounded identity slots; this API does
not claim a complete expiry pass per invocation.

The expiry step has a cooperative four-second sub-budget. Failure reports
`maintenance_deferred` and still permits useful mailbox pumping. The rest of
the window accepts the same native durable round routes, including signing
phases of a retained session. Empty pulls keep waiting for work until the
window deadline or batch limit. The result's maintenance counters describe
attempted owners, never global completion or expiry protection.

The host should attempt scheduled maintenance as well as push-triggered work:
local expiry need not produce any server message. Push delivery, coalescing and
host scheduling do not guarantee a second wake before a round deadline. Cold
sessions retain the runtime's authoritative recovery hold; this API does not
reconstruct lost signing nonces or bypass wallet unlock.

## Lifetime ownership

`CancelReceiptWindow` signals the matching ID without releasing its owner.
It remembers the last cancellation, including cancellation just before
admission. Hosts use unique IDs and serialize their notification callbacks.
Returning foreground cancels an admitted window before waiting for its final
access, then starts normal ingress on the daemon context. The short caller
context is linked only during synchronous startup. Canceling it after return
cannot stop foreground ingress. Pause releases the retained foreground child. An already-canceled mode call
leaves the current mode alone. A foreground transition that fails after it
begins leaves ingress paused. The host must retry foreground startup or perform
its ordinary owned restart before making new wallet requests. Connectivity
publication shares the serialized mode transition, with false on failure; an
immediate incompatibility callback can still override startup to false.

Every new SDK callback uses the same admission and final-access join as other
embedded callbacks. Close cancels them and joins before daemon cleanup. An
uncooperative edge or callback may exceed a caller's deadline; the handle
remains owned. The host must never Stop/reopen its data directory from an
expiry callback. This controller promises neither safe-to-suspend nor a hard
wall-clock cap on uncooperative dependencies.

## Compatibility and rollback

Remote SDK handles reject these APIs. Builds without swap/wallet integration
cannot supply source recovery. Old hosts keep continuous ingress and default
round behavior. Updated hosts must opt into durable rounds before mode
control is available.

The previous mobile source using schema 28 cannot reopen the new schema 29.
The new runtime also refuses disabling durable round mode after adoption.
An older schema-29 binary that does not understand native round ownership is
not a supported rollback. Use a compatible forward fix and preserve the wallet
files. Notification revocation must not switch off durable round mode.

## Verification

Run the focused race regressions with:

    go test -race -tags 'mobile,wavewalletrpc,swapruntime,test_sqlite' \
      ./waved ./serverconn ./mailbox/conn ./swapclientserver \
      ./swapwallet ./sdk/wavewalletdk ./sdk/wavewalletdk/mobile \
      -run 'TestMaintenance|TestMaintainExpiry|TestReceiptWindow|TestRestrictedPump|TestCloseJoinsLocalCallbacks|TestTargetedReceiveResume'

They hold the pump's last access beyond cancellation, verify foreground cannot
escape it, cancel before admission, preserve foreground lifetime after caller
return, distinguish source completion from an empty mailbox, refuse a whole
mixed batch before waiter delivery, and re-arm a targeted receive without a
historical scan or pay worker.
