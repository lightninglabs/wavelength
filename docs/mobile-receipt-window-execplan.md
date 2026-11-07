# Deliver warm invoice recovery windows

This plan follows `PLANS.md`. Keep its progress, discoveries, decisions and
outcomes current. A host should be able to recover one newly enrolled Bitcoin
invoice after an authorized hint, while retaining exclusive daemon ownership.

## Progress

- [x] Add send-time response provenance and invocation-local ingress policy.
- [x] Add targeted existing-receive resumption without a historical scan.
- [x] Add serialized warm ingress modes and bounded source polling.
- [x] Expose owned SDK/mobile callbacks and durable-runtime configuration.
- [x] Verify focused cancellation, final-access and source regressions.
- [x] Complete affected race suites, changed lint, docs and new-declaration audit.
- [ ] Publish the signed SDK-only change.
- [ ] Complete Gateway review at the final head.

## Repository and execution map

`mailbox/conn/response_registry.go` owns a waiter's original request method.
`serverconn/ingress_pump.go` snapshots a per-call policy. `ingress.go` applies
it after sender/version checks and before any dispatch. `waved/receipt_window.go`
serializes ingress ownership and polls one local invoice. `swapwallet` validates
and resumes the typed source; `swapclientserver` reuses its receive-worker gate.
`sdk/wavewalletdk` holds direct-call admission until final access, and its mobile
subpackage provides JSON/scalar methods with cooperative budgets.

## Milestones and validation

First prove that a pulled mixed batch cannot deliver even an earlier allowed
response when a later route needs foreground. Prove a manual or signing waiter
cannot impersonate a permitted read; an actual native request-kind durable
handoff remains allowed. Run the `TestRestrictedPump` race tests.

Next prove cancellation before admission, foreground takeover while the old
pump still holds its last access, and persistent foreground context after its
caller returns. Run `TestReceiptWindow`. An empty pull must not convert a
pending source into completion. A targeted resume must not list history or
start a pay worker; run `TestTargetedReceiveResume`.

Finally run the documented affected package race suites, SDK callback final-
access regression, default and tagged lint, `make doc-check`, GoDoc audit and
clean module checks. Publish a signed SDK-only PR and request actual Gateway
review. No deploy or native artifact publication belongs to this plan.

## Surprises & Discoveries

Go synctest treats mutex waits as non-durable. The foreground final-access
regression uses an explicit cancellation handshake and blocked access barrier
with a short real-time exclusion check. Its ordinary source polling test can
use virtual time. An empty mailbox is not a source-settlement barrier.

## Decision Log

Retain daemon/domain workers across windows. Pausing intake does not claim
safe suspension. Restrict whole batches before volatile dispatch. Keep exact
read-method provenance inside the existing waiter so TTL and removal share
one owner. Re-arm only already-admitted invoice sources. Keep foreground
context on the daemon root and unlink the startup caller on return. Keep the
durable-runtime setting independent of notification consent.

## Outcomes & Retrospective

Full affected race suites pass. New declarations have documentation; 15
pre-existing missing comments remain unchanged. Default whole-file changed
lint and tagged changed-line lint pass. The tagged whole-file run exposes
10 unchanged baseline issues in old swap helpers/configuration. Signed
publication and external review remain. The
separate host must gate amount presentation on an authorized matching event,
consent, account/network identity and durable deduplication. Cold unlock,
arbitrary activity accounting and guaranteed background execution are outside
this controller.
