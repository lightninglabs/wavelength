# Wallet work that needs a wake

A producer hint says existing wallet work is ready. The host authenticates its
local route and runs the wallet. A push never authorizes signing, selects an
arbitrary wallet or proves completion.

## Work inventory

| Ready event | Client work | Delivery |
| --- | --- | --- |
| Consented registration window | Join an eligible queued round, refresh, boarding or cooperative leave | One scheduled job per binding/cutoff, inside the published window. |
| Quote/requote, batch data, aggregate nonces/signatures or input-signature phase | Validate terms/tree and provide the required signature before the phase expires | Round class, independent five-second coalescing, deadline expiry and queue priority. |
| Admission, terminal round result or actionable status | Reconcile the existing attempt | Work class; no signature deadline is inferred from admission or failure. |
| Out-of-round submit/finalize event | Validate co-signatures, sign checkpoints and apply settlement | Work class. These are protocol events, even when logically replying to a prior turn. |
| Incoming transfer/VTXO or confirmed round | Verify and materialize received outputs | Work class, including durable offline source hints. |
| Held Lightning payment | Verify terms and persist the acceptance that permits funding | Work class; optional payment presentation is separate. |
| Funded receive or internal receive | Validate the actual contract and claim with the existing key/session | A fresh producer work hint is needed if funding outlasts an earlier host slice. |
| Receiver participant-signature request | Validate the transcript and sign the existing obligation | Work class. The receive responder must actually be running; pumping another connection alone is insufficient. |
| Credit receive or ready continuation | Apply committed receipt/state and continue an authorized operation | Work class. Polling alone creates no new wake. |
| Locally due VTXO refresh | Sample current tip and drive the existing expiry policy | Host maintenance and the schedule heartbeat supply opportunities. No server event is guaranteed for local due work. |

Do not wake for routine query responses, heartbeats, consumed duplicates,
cosmetic history refresh or server-only work. Waiting for a confirmation, proof
or maturity needs a new wake when useful client action becomes possible, not
on every block or poll. Source responses must stay suppressed to prevent
wake → request → response → wake loops.

## Relay pacing and authority

Work hints coalesce for five seconds per installation after accepted sends.
New work therefore waits seconds rather than the former twenty-minute
maintenance interval. A newer generation survives completion of an older send.
Same-generation feed replay creates no new job. There remains one provider
request owner per installation, with unchanged failure backoff.

Round consent is explicitly signed. It enables short-lived signing hints and
scheduled opportunities. A previous scheduled push does not suppress the next
consented slot. Duplicate schedule steps and restarts cannot enqueue the same
binding/cutoff twice. Dense timetables and multiple bindings increase requests;
the publisher must measure the OS's shared per-app budget on real devices.

Payment banners have independent opt-in and a one-minute presentation quiet
period. Work must not depend on an optional banner or amount preference. A
payment hint is not a receipt; only committed canonical wallet state can prove
a completed amount. Actual user opt-out remains authoritative.

All existing signed grants, source identity, recipient binding, expiry,
fee/script/timelock checks, accepted-effect reconciliation and nonce fences
remain in force. A notification payload is never an invoice source reference.

## Native execution and limits

The host serializes windows and retains the daemon through final native access.
Overlapping callbacks need a bounded handoff, including when the current
payment receipt is already complete and returns without pumping. Cancelling a
window is not permission to reopen its data directory.

A maintenance window first pumps ready protocol messages, then runs the same
bounded expiry scan once and continues pumping until its host deadline. If the
first pump fails or requires foreground, maintenance remains deferred. This
reorders work; it adds no signing permission or native owner.

Receive work needs the existing worker. `RunWorkWindow` takes at most 64 trusted
local receive references and re-arms them after the first pump, even if that
pump needs foreground. It reads no receipt and returns no amount proof.
Cold startup keeps its wallet-ready resume hooks. Legacy generic maintenance
with zero references cannot re-arm an exited warm receive. Hosts must persist
work references independently of optional banners, retain obligations beyond
one finite page and reconcile them in foreground. Legacy invoices lacking a
saved reference retain cold/foreground recovery. Physical warm/cold execution
still needs a device test.

The host must state its selected-wallet/network policy. Push data must not
choose a new wallet, identity or endpoint. This SDK change adds no inactive
wallet executor or multi-wallet ownership model.

Local exit, CSV sweep, boarding and swap recovery timers do not themselves wake
a suspended phone. Authenticated cold unroll/boarding replay still requires its
existing explicit foreground path. A complete bounded background safety
executor is separate work. Preserve durable obligations and foreground
reconciliation; do not infer funds safety or a completed refresh from a callback.

## Validation layers

Unit/SQL tests establish source classification, generation replay, leases and
eligibility. Process integration establishes source/feed/relay boundaries.
Neither establishes APNs delivery or a locked phone's signature. Device
acceptance must correlate a real source event, subscriber commit, provider
attempt, callback/window and completed client operation.

See [feed consent](notification-feed.md), [relay ownership](notification-relay.md)
and [native execution windows](mobile-receipt-window.md).
