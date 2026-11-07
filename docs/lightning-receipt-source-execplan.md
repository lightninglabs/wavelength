# Targeted Bitcoin invoice receipt reads

This plan follows PLANS.md. It supplies the local source boundary for exact
amount notifications. It does not activate notifications or deploy a service.

## Purpose

A push and a positive activity amount do not prove a new Bitcoin receipt.
Check only explicitly enrolled invoices against their typed durable receive
source, then project committed local state through the canonical sink.

## Progress

- [x] Trace normal and credit receive identity, principal and original time.
- [x] Separate invoice receives from OOR/asset carriers and internal funding.
- [x] Implement targeted local source lookup and canonical projection.
- [x] Expose embedded Go/mobile calls under SDK final-access ownership.
- [x] Verify real invoice binding, terminal state, historical creation time,
  credit principal, wrong direction, pending progress and shutdown joining.
- [ ] Format, lint, test, document, sign and request Gateway review.

## Contract

The host enrolls the typed reference returned by a successful Receive after
explicit consent. Lost enrollment can omit an alert; it cannot lose funds.
Source creation must be strictly after the host's opt-in second. The source
stores have one-second precision, so equality is deliberately suppressed.

At most 64 references are checked per call. Each source uses a targeted read,
not a full credit/history list. The source's BOLT-11 must match the wallet
network, hash and principal. The canonical row must agree with the normalized
receive before amount presentation. Pending local sources remain pending; the
method does not pretend to have awaited a remote reconciliation turn.

This deliberately does not classify raw Ark inventory, aggregate boarding
addresses or asset carrier values. Those require their own provenance and
receipt identities. No universal history baseline or safe-to-suspend result.

## Validation

Full affected race suites: swapwallet 26.573s, waved 84.365s, SDK 37.124s,
mobile 2.124s. The first SDK run exposed an incomplete test fixture; setting
its wallet-RPC capability made the actual Receive path available. No production
behavior was changed for that fixture failure. PostgreSQL receipt cases pass
in 7.877s. Default SDK tests pass in 4.908s. Real BOLT-11 fixtures exercise
principal, hash, network, pending/completion and wrong-direction cases. Real
credit SQL verifies immutable creation time across updates and late first
projection. The direct-callback final-access regression now covers all seven
SDK call sites, including this one.

Tagged lint is clean. Default lint, documentation links and final GoDoc checks
are required before the signed commit. Local tests use serial package linking
and omit debug symbols to fit the available disk; race instrumentation stays
enabled. No native artifact is published by this PR.
