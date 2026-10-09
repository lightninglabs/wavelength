# Receive Forfeit Signing Authority Specification

Status: model-derived implementation contract.

Model SHA-256:
`8708a16b7a9a8cadf726cdaa309214bced7944b9f767a38b26d82a3432f83733`

## Scope

This document specifies when a receive-side mailbox responder may invoke the
forfeit signing oracle and how the connector-bound broker handles redelivery.
The P model is authoritative for ordering, binding admission, replay, and the
abstract restart distinction. Production code and its cryptographic libraries
remain authoritative for wire encoding, policy decoding, sighash construction,
participant keys, and Schnorr verification.

The key words **MUST**, **MUST NOT**, and **MAY** are interpreted as described
by RFC 2119 and RFC 8174 when they appear in uppercase.

- Requirement: `FORFEIT-SIGN-000`
- Model: unmodeled; this paragraph defines the document convention.

## Actors and State

The receive session owns an immutable funded binding containing the payment
hash, vHTLC outpoint, amount, pkScript, policy template, and the payment hash
embedded by that policy. The mailbox responder owns a read-only view of the
published binding. The local signing oracle signs the exact connector-bound
transaction. The daemon broker retains the first accepted remote participant
signature set for each request id.

The model has one running state and handles three environment actions:
publication of a binding, crash/restart, and delivery of a signing request.
The environment chooses whether publication is durable, whether a submitted
signature is valid, and whether the mailbox acknowledgement succeeds.
`ePublishFunding` represents a completed persistence step. A failed store
write does not emit that event; the production bridge exercises that
precondition by failing persistence and delivering the request before a
successful retry.

## Requirements

The mailbox responder MUST NOT invoke the signing oracle until the receive
session has published a complete funded binding after its configured
persistence step succeeds.

- Requirement: `FORFEIT-SIGN-001`
- Model: `src/forfeit_signing.p:118`; monitor:
  `src/forfeit_signing.p:172`.

Before invoking the signing oracle, the responder MUST require the request's
payment hash, vHTLC outpoint, amount, pkScript, policy template, and
policy-embedded payment hash to match the published funded binding.

- Requirement: `FORFEIT-SIGN-002`
- Model: `src/forfeit_signing.p:70`; monitor:
  `src/forfeit_signing.p:172`.

A request rejected before signing and a request whose post-submission mailbox
acknowledgement fails MUST remain unacknowledged so that transport redelivery
can retry it.

- Requirement: `FORFEIT-SIGN-003`
- Model: `src/forfeit_signing.p:118` and
  `src/forfeit_signing.p:161`; scenario:
  `test/forfeit_signing_test.p:109`.

For a retained request, the broker MAY accept a different signature encoding
on replay only when the new signature independently verifies for the same
transcript and participant key set.

- Requirement: `FORFEIT-SIGN-004`
- Model: `src/forfeit_signing.p:146`; signature verification is unmodeled and
  is exercised by the Go broker bridge.

After accepting the first signature set for a request id, the broker MUST NOT
replace that retained set when it accepts a valid replay.

- Requirement: `FORFEIT-SIGN-005`
- Model: `src/forfeit_signing.p:146`; monitor:
  `src/forfeit_signing.p:210`.

After restart, a client with a configured durable store MUST reconstruct the
published funded binding from persisted receive-session state. A process-local
client MUST require publication again before it can sign.

- Requirement: `FORFEIT-SIGN-006`
- Model: `src/forfeit_signing.p:101`; scenarios:
  `test/forfeit_signing_test.p:109` and
  `test/forfeit_signing_test.p:211`.

## Failure and Retry Behavior

Rejection before signing does not acknowledge the mailbox request. Successful
submission followed by acknowledgement failure can therefore invoke the local
signer and broker again. Schnorr signing may produce different bytes on that
second invocation. The broker validates the new bytes against its retained
connector transcript and participant keys, accepts a valid answer
idempotently, and preserves the original retained signature set.

An invalid signature or unexpected participant key is rejected. A request id
that has aged out of the broker's bounded answered-request window is unknown;
rejecting that late replay does not weaken the signing-authority or
first-answer safety properties.

## Safety Properties and Assumptions

`SigningRequiresPublishedExactAuthority` observes every signer invocation and
asserts both publication and an exact binding match.
`BrokerReplayPreservesFirstSignature` observes every accepted submission and
asserts that later submissions for the same lifecycle and request id retain
the first signature.

The model makes no liveness claim. Eventual mailbox redelivery, backend
availability, and a successful acknowledgement are environment assumptions.
A green run establishes these safety properties only for the explored model
and checker bounds.

## Conformance Matrix

| Requirement | P model | Monitor/property | Production code | Tests/traces | Status |
|---|---|---|---|---|---|
| `FORFEIT-SIGN-001` | `ForfeitSigningLifecycle` | `SigningRequiresPublishedExactAuthority` | `receiveForfeitBindingGate.load` | `session_authority_replay.json` | verified |
| `FORFEIT-SIGN-002` | `ExactBinding` | `SigningRequiresPublishedExactAuthority` | `validateOutSwapForfeitSignaturePayload` | session bridge and package unit tests | verified |
| `FORFEIT-SIGN-003` | rejected and unacked results | green scenario assertions | `handleOutSwapForfeitSignatureRequest` | `session_authority_replay.json` | verified |
| `FORFEIT-SIGN-004` | validity boolean abstraction | accepted replay path | `forfeitSignatureBroker.submit` | real Schnorr broker bridge | partially verified: cryptography is outside P |
| `FORFEIT-SIGN-005` | retained signature map | `BrokerReplayPreservesFirstSignature` | `forfeitSignatureRequest.signatures` | `broker_valid_replay.json` | verified |
| `FORFEIT-SIGN-006` | durable publication flag | crash/restart scenarios | `ResumeReceiveViaLightning` | `session_authority_replay.json` | verified |

## Abstractions and Open Questions

The model represents each identity field as an integer and signature validity
as an environment boolean. The bridge closes that gap with the production
policy decoder and real Schnorr signatures, but neither layer independently
proves the cryptographic libraries. The P broker retains answered requests
without a bound; production prunes old answered requests. The model also does
not encode mailbox wire serialization, transaction construction, process
scheduling fairness, chain reorganizations, or backend availability.
