# Publisher-owned notification relay

## Purpose

Build a standalone relay that lets an attested app register its APNs device
token and issue revocable wake/alert capabilities to a mailbox operator. The
operator sees neither device tokens nor APNs provider credentials. Requests
cannot retarget a capability, select an app topic or supply arbitrary alerts.
This PR prepares source and tests; it performs no deployment or live push.

## Progress

- [x] Verify Apple's current App Attest and APNs contracts from official docs.
- [x] Select a pinned App Attest verifier with Apple's validation-guide vectors.
- [x] Define the cross-language signed transcript and publisher app policy.
- [x] Implement atomic authenticated challenge use, installation counters, subscriptions and
  durable coalesced jobs in a dedicated Bolt store.
- [x] Add fixed-template APNs delivery, provider JWTs and token-revision fencing.
- [x] Add bounded HTTP endpoints, the command, container and API/runbook docs.
- [x] Prove auth/replay/scope/revocation/token rotation/retry/restart with fakes.
- [ ] Complete formatting, lint, signed commits, PR and Gateway review.

## Decisions

Keep this service in the public client repository, separate from wallet/runtime
code and independent of operator internals. Use a dedicated Bolt database and
single service instance with persistent storage. App Attest controls enrollment
and management. An opaque random bearer capability authorizes one scope for
one subscription and fixed app-owned route. Store capability hashes only.

Anonymous challenges are stateless, MAC-bound and expire after five minutes. The database key survives restart. Only a cryptographically authenticated use enters the bounded replay set, atomically with its counter and mutation. Anonymous requests cannot exhaust a persistent nonce table.

The signed client data is a newline-delimited domain, operation, server app
alias, installation ID, challenge ID, server nonce and SHA256 of exact payload
bytes, with a final newline. The payload is base64 in the proof envelope, so
Swift and Go need not canonicalize JSON identically. Challenge use and strictly
monotonic assertion counter updates commit with the authorized mutation.

App aliases fix the Apple App ID, topic, App Attest environment and APNs
environment. TestFlight uses a production APNs alias independently of Bitcoin
network. Production cannot accept development attestations through that alias.

Wake jobs coalesce per subscription. Alert jobs deduplicate per opaque event
identity and use a fixed generic payment template. Amounts belong to verified
local activity in the app. Jobs and retries are bounded by expiry. An old APNs
invalid-token response cannot disable a token registered at a newer revision.

## Validation and acceptance

Tests must cover changed signed payloads and audiences, replayed challenges and
counters, concurrent management, wrong-scope/revoked/expired capabilities,
fixed destinations, rotation during delivery, coalescing during a leased send,
provider failures, expiry and reopening the same database. HTTP and APNs tests
use fakes; no test sends to an Apple device. Source review cannot establish
real-device App Attest, APNs delivery or iOS execution grants.

## Outcomes

Implementation is complete; validation and PR publication are in progress. No deployment exists. The
separate SDK foundation is #1531; its architecture document is #1532. Further
PRs must provide bounded SDK execution, authenticated mailbox subscription,
operator jobs, language wrappers, iOS flows and inactive infra configuration.

## Context and milestones

This plan follows `PLANS.md`. `notifyrelay/api.go` defines the JSON contract;
`auth.go` verifies Apple proofs; `store.go` owns the dedicated Bolt file;
`jobs.go` owns coalescing, leases and bounded maintenance; `apns.go` owns the
fixed provider transport. `server.go` and `http.go` connect these boundaries.
`cmd/wavenotifyd/main.go` owns process cancellation and storage shutdown.

The first milestone builds authenticated storage: altered payloads, audiences,
counters and consumed nonces must fail through the HTTP API. The second adds
coalesced provider delivery: rotation and concurrent new events must survive
old completions. The final milestone adds process ownership, a non-root image
and the API/runbook document, then publishes a signed PR for Gateway review.

## Concrete steps

From the repository root, run:

    go test -race ./notifyrelay ./cmd/wavenotifyd -count=1 -timeout=90s
    go test github.com/takimoto3/app-attest -run 'Test.*Golden|TestApple'
    make fmt-changed
    make lint-changed-local
    make tidy-module-check

The first command must report `ok` for both packages. It uses a real Bolt file,
real assertion signatures and fake provider transports. The command shutdown
test reopens the same file after a successful join. The verifier's golden tests
exercise Apple's published validation-guide material. Neither test command
contacts a device or sends a push. Build the command with `go build
./cmd/wavenotifyd`; deployment configuration and Apple credentials remain
separate acceptance gates.

## Surprises & Discoveries

Apple assertion verification hashes the nonce before ECDSA verification. The
pinned library's golden vectors and real-signature management tests pass with
that contract. A passing enrollment fake alone would not establish it.

Provider errors can contain device URLs. The APNs adapter emits sanitized
errors. Explicit permanent outcomes must survive an accompanying diagnostic
error; `TestPermanentProviderFailureRetires` protects this distinction.

## Decision Log

Decision: stateless MAC challenges with authenticated consumption only.
Rationale: anonymous nonce requests must not allocate an unbounded persistent
replay table. The MAC key survives process restart and counters remain atomic.
Date: 2026-11-07.

Decision: one process and a dedicated Bolt file.
Rationale: the initial service needs durable retry and atomic counter updates,
without introducing distributed claims or a shared wallet-storage dependency.
The file lock enforces ownership. Date: 2026-11-07.

Decision: fixed generic remote alerts and locally verified amounts.
Rationale: mailbox activity is not proof of received value. The app must verify
and persist the receipt before stating an amount. Date: 2026-11-07.

## Idempotence and recovery

Tests use temporary directories and may be repeated. Source rollback does not
remove the relay file. Do not reset counters or the challenge key on restart.
A lost enrollment reply recovers via the derived installation identity; a lost
management reply recovers via a fresh challenge/assertion and route renewal.
A provider timeout retries its existing durable job. Never remove the live
file or close it while HTTP handlers or the worker still own operations.

## Outcomes & Retrospective

Authentication, provider transport, queue recovery, HTTP service, command,
container and runbook are implemented. Local package and race tests pass;
formatting, lint and publication checks are in progress. Real-device App
Attest, push delivery, operator integration and iOS wallet execution remain
separate integration work. No live credential or deployment was created.

Revision 2026-11-07: record implemented boundaries, stateless challenges,
provider error classification and the exact repeatable local acceptance steps.

Gateway review of the first published head found no blocker and returned
READY WITH FOLLOW-UPS. F2's current-revision/older-timestamp APNs rejection now
retires its job without disabling the installation. Its regression protects
that decision separately from the existing replaced-revision retry test.
F1's bounded Bolt bucket scans remain a performance follow-up: profile before
raising capacity, then consider transactional metadata counters and a due-job
index together. Adding counters alone would not remove the claim scan.


Revision 2026-11-07: add read-only issued-grant verification for producers.
A signed mailbox identity cannot prove that opaque capability bytes came from
an attested installation. `/v1/subscriptions/verify` checks wake/subscription
binding, optional alert pairing, expiry, revocation, installation and publisher
policy without creating a job. Producers verify it before persisting a route;
the call stays outside their database transaction. Regression tests prove
mixed/stale grants fail and successful verification sends no notification.
Gateway re-review is required after this additive API change.


Revision 2026-11-07: prepare relay image publication in the existing Docker
workflow. Reuse its registry login and tag suffixes, select the dedicated
Dockerfile and embed the actual checked-out SHA. Local actionlint and all four
trigger-shape tag checks pass. No publication workflow was run; main merge and
normal source publication remain separate from inactive-chart activation.
