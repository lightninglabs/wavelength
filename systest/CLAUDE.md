# systest

## Purpose

System-level end-to-end integration tests exercising the full daemon with real
Bitcoin/LND backends via the test harness.

## Key Tests

- `TestSendVTXOEndToEnd` — exercises directed in-round send through the full
  stack: gRPC, wallet, VTXO manager, round FSM, and mailbox JoinRound egress.
- `TestSendOORMultipleRecipientsEndToEnd` — exercises multi-recipient `SendOOR`
  through the full daemon stack, asserting at the fake-operator mailbox
  boundary that one session is built with both recipient outputs in the
  SubmitPackage payload.

## Relationships

- **Depends on**: `harness` (test environment), `darepod` (daemon under test),
  `rpc/oorpb` (OOR mailbox payload decoding).
- **Depended on by**: nothing (test-only).
