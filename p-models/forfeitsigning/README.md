# Receive Forfeit Signing Model

This P project models the authority and retry boundaries for a receive-side
forfeit signature request. The mailbox carries a request, but it does not grant
signing authority. Authority comes from the receive session's immutable funded
binding after its configured persistence step succeeds.

The result is model-derived within the bounds below. It is not a proof of the
wire encoding, the transaction sighash construction, or Schnorr verification.
Those remain implementation and cryptographic-library obligations exercised by
the Go bridge.

The normative, model-derived contract and traceability matrix are in
[`SPEC.md`](SPEC.md).

## Requirements

- **FS-AUTH-1:** The signing oracle MUST NOT be invoked until the receive
  session publishes a complete funded binding.
- **FS-AUTH-2:** The request payment hash, vHTLC outpoint, amount, pkScript,
  policy template, and policy-embedded payment hash MUST match the published
  binding before the signing oracle is invoked.
- **FS-RETRY-1:** A rejected request or a request whose acknowledgement fails
  MUST remain eligible for mailbox redelivery.
- **FS-REPLAY-1:** Once the broker accepts a signature set for a retained
  request, a replay MAY submit different signature bytes only when they
  independently verify for the same transcript and participant keys.
- **FS-REPLAY-2:** A valid replay MUST NOT replace the first signature set the
  broker delivered to the original waiter.
- **FS-RESTART-1:** A configured durable store MUST reconstruct a published
  binding after restart. A process-local binding MUST be unpublished after
  restart until the receive session establishes authority again.

`SigningRequiresPublishedExactAuthority` checks FS-AUTH-1 and FS-AUTH-2.
`BrokerReplayPreservesFirstSignature` checks FS-REPLAY-1 and FS-REPLAY-2 for
accepted submissions. The green driver covers rejection before authority,
publication, acknowledgement failure, alternate-valid replay, identity drift,
invalid replay, and durable versus process-local restart behavior.

`tcForfeitRequestDerivedCounterexample` deliberately selects the old unsafe
rule in which the request supplies its own authority. The test must find a bug
when that rule invokes the signer before any funded binding exists.

## Implementation Bridge

The checked-in JSON traces are replayed at both production boundaries:

- `sdk/swaps/forfeit_model_bridge_test.go` drives the real receive-session
  binding gate and mailbox responder. It verifies that signer and submit calls
  are absent before authority and on identity drift, present after publication,
  retried after acknowledgement failure, and restored from the durable store
  after restart. Identity drift covers the five request fields plus both
  policy-to-payment-hash consistency relations.
- `waved/forfeit_model_bridge_test.go` drives the real connector-bound broker
  with actual Schnorr signatures. It verifies same-byte replay,
  alternate-valid replay, invalid signatures, wrong participant keys, and
  retention of the first accepted signature set.

| Requirement | P source | Concrete trace | Production boundary |
| --- | --- | --- | --- |
| FS-AUTH-1 | `SigningRequiresPublishedExactAuthority` | `session_authority_replay.json` | `receiveForfeitBindingGate.load` |
| FS-AUTH-2 | `ExactBinding` | `session_authority_replay.json` | `validateOutSwapForfeitSignaturePayload` |
| FS-RETRY-1 | green driver acknowledgement failure | `session_authority_replay.json` | `receiveForfeitResponder.handleOutSwapForfeitSignatureRequest` |
| FS-REPLAY-1 | `ForfeitSigningLifecycle.HandleSigningRequest` | `broker_valid_replay.json` | `forfeitSignatureBroker.submit` |
| FS-REPLAY-2 | `BrokerReplayPreservesFirstSignature` | `broker_valid_replay.json` | retained `forfeitSignatureRequest.signatures` |
| FS-RESTART-1 | green driver crash/restart cases | `session_authority_replay.json` | `ResumeReceiveViaLightning` and binding publication |

## Bounds and Assumptions

The model abstracts each identity field to an integer and signature validity to
a boolean supplied by the cryptographic boundary. The Go bridge covers the
real policy decoder and real Schnorr verification. The model keeps a retained
broker request indefinitely; production bounds answered-request retention, so
a replay after pruning returns not found and does not affect safety. The model
contains no liveness claim: mailbox redelivery, backend availability, and a
successful acknowledgement are environment assumptions. `ePublishFunding`
means the configured persistence step has already succeeded; the session
bridge separately fails the real store boundary and proves that delivery stays
unsigned before the successful retry.

Run this project through the repository entrypoint:

```shell
./p-models/scripts/check.sh
```

For a focused green check:

```shell
p compile -pp p-models/forfeitsigning/infra.pproj
p check PGenerated/PChecker/net8.0/ForfeitSigningModels.dll \
  --testcase tcForfeitSigningAuthorityAndReplay \
  --schedules 100 \
  --max-steps 500
```
