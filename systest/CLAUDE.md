# systest

## Purpose

System-level end-to-end integration tests exercising the full daemon with real
Bitcoin/LND backends via the test harness.

## Key Test Infrastructure

- `fakeMailboxServer` — in-process mailbox stub capturing envelopes for
  assertion. Captures both `roundpb.JoinRoundRequest` envelopes and, since
  darepo-client#708, `oorpb.SubmitPackageRequest` envelopes via
  `recordOORSubmitPackage`. Tests can assert on the real mailbox payloads
  darepod emits without a live server.

## Relationships

- **Depends on**: `harness` (test environment), `darepod` (daemon under test).
- **Depended on by**: nothing (test-only).
