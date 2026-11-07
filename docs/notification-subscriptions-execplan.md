# Authorize mailbox notification subscriptions

This plan follows `PLANS.md`. Keep progress, discoveries, decisions and outcomes
current as the shared control protocol and SDK bridge are reviewed.

## Purpose

An enrolled app can authorize its operator to notify one fixed relay route
without exposing wallet keys or relying on a captured static ownership proof.
A changed destination, capability, expiry or operator must invalidate the
signature. The app can remove its route and recover an ambiguous registration
response by signing a fresh idempotent mutation.

## Progress

- [x] Define additive direct edge challenge and subscription RPCs.
- [x] Generate protobuf/gRPC bindings with the canonical Docker workflow.
- [x] Implement stateless challenge and full-request BIP-340 helpers.
- [x] Bridge the embedded wallet signer through Go and gomobile APIs.
- [x] Prove field/domain binding, expiry, server-key restoration and old-edge fallback.
- [x] Complete formatting, tagged builds, lint and declaration documentation audit.
- [ ] Publish the signed PR and complete Gateway review.
- [ ] Integrate the contract in the separately reviewed operator and native app.

## Context and milestones

`mailbox/pb/notification.proto` defines the direct service. The `notification`
package owns canonical authorization and challenge helpers without owning a
database. `waved/notification.go` borrows the bootstrapped operator connection
and wallet signer. `sdk/wavewalletdk/notification.go` exposes a wallet-shaped
DTO; `sdk/wavewalletdk/mobile/notification.go` exposes bounded JSON calls.

The first milestone establishes wire and proof contracts. The second connects
the actual embedded signer to that service and proves no ingress pump is
needed for its reply. The final milestone documents the operator's atomic
replay/subscription boundary, validates tagged builds and requests review.

## Concrete steps and acceptance

From the repository root, run `make rpc` to regenerate the new service. Expect
only the new notification generated files, with no hand edits. Run:

    go test -race ./notification ./sdk/wavewalletdk -count=1 -timeout=3m
    go test -tags='mobile wavewalletrpc swapruntime test_sqlite' ./sdk/wavewalletdk/mobile ./waved -run 'TestNotification|TestParseConfig|TestStartExternalSeed|TestClientStop' -count=1 -timeout=3m
    make fmt-changed
    make lint-changed-local

Real Schnorr signatures must verify unchanged and fail after changing each
signed authority/routing field, using a static mailbox tag or adding unknown
v1 fields. Restoring the same persistent server secret verifies its previous
challenge; another secret, another operator and expiry fail. The direct client
must reject a wrong negotiated operator before calling the signer, accept
upsert/removal and report unsupported on an old edge. Tests run without a live
operator, relay, Apple credential or device.

## Surprises & Discoveries

The existing mailbox RPC facade waits for responses through ingress. A direct
edge control service avoids that circular dependency for background setup.
The existing daemon signer already accepts raw message plus BIP-340 tag for
all supported wallet backends, so this feature needs no key extraction.

## Decision Log

Decision: direct additive mailbox-edge service and full-request signatures.
Reason: notification management is a short control operation and must complete
while ingress is paused. Static mailbox auth proves key control but does not
bind a new destination. Date: 2026-11-07.

Decision: shared stateless MAC challenge helpers, with database consumption
left at the operator's owning boundary. Reason: anonymous requests must not
fill durable nonce tables; only a verified mutation creates replay state and
that replay state must commit atomically with the subscription. Date: 2026-11-07.

Decision: embedded SDK bridge only. Reason: the daemon already owns the signer
and negotiated connection. A new remote signing API is unnecessary for the
native app and would expand the authorization surface. Date: 2026-11-07.

## Idempotence and recovery

Generation and tests may be repeated. Preserve existing generated outputs that
are unrelated to this proto. An ambiguous mutation response is retried with a
new challenge, same logical subscription and exact desired capabilities.
Reopening an operator must retain its server challenge secret and replay rows.
Subscription and replay persistence are separate server implementation work;
these helpers never claim that verification alone prevents replay.

## Outcomes & Retrospective

Wire definitions, shared helpers and the embedded Go/gomobile bridge are
implemented. Local race, SDK, full tagged mobile tests, formatting, declaration audit and
changed-code lint pass. Publication and review are in progress. The operator must implement
the atomic replay/mutation/durable delivery contract; the app must enroll with
the relay first. No deployment or new credential was created. This PR does not
establish bounded whole-wallet execution or notification presentation.

Revision 2026-11-07: record the direct-control choice, canonical transcript,
backend signing reuse and explicit server persistence obligations.


Gateway review found client clock coupling in transcript construction. The
encoder now checks shape only; server verification retains challenge freshness
and bounded subscription expiry policy. A real-signature test runs the issuer
one minute ahead of the client. Relay expiry policy permits five minutes of
operator/relay skew and still requires the stored route to be capped to the
relay-verified grant. The daemon no longer waits for ingress-inclusive wallet
services readiness. A real bufconn RPC test uses its LND signing bridge with
readiness deliberately unresolved and no ingress actors.

Revision 2026-11-07: address Gateway F1 clock skew, F2 relay expiry tolerance and
F3 ingress-independent daemon signing; local focused race tests pass.
