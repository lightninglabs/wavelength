# systest

## Purpose

System-level end-to-end integration tests exercising the full daemon with real
Bitcoin/LND backends via the test harness.

## Test Coverage

- `TestOORIncomingMaterializationSpawnsVTXOActor` — single incoming OOR
  receive materializes a VTXO and spawns its actor.
- `TestOORSelfChangeMaterializationSkipsExternalRecipient` — self-change
  OOR materialization ignores external-recipient outputs.
- `TestOORConcurrentIncomingMaterialization` — drives N independent
  incoming OOR receives concurrently; verifies per-session actors make
  parallel progress (coverage for issue #605).
- `TestSendVTXOEndToEnd` — single-recipient SendOOR through the gRPC
  surface end-to-end.
- `TestSendOORMultipleRecipientsEndToEnd` — multi-recipient SendOOR via
  daemon gRPC; verifies one package contains all requested outputs.
- `TestLeaveStrandedVTXORecoversOnAdmissionTimeout` — stranded leave
  VTXO recovers after admission timeout.
- `TestBoardingWalletIntentPersistence`, `TestBoardingWalletBacklogNotifications`,
  `TestBoardingWalletAddressReuse`, `TestBoardingWalletMultipleAddresses` —
  boarding wallet lifecycle and notification coverage.
- `TestVHTLCRecoveryRPCEndToEnd` — VHTLC recovery RPC lifecycle.

## Key Helpers

- `driveIncomingOutbox` — synchronous driver for the OOR receive-flow
  outbox; handles `ScheduleRetryRequest` by ignoring it (metadata is
  resolved immediately in tests so the retry timer never fires).
- `seedIncomingRound` — seeds a round in the store for incoming
  materialization tests.
- `buildSystemTestIncomingMaterialization` / `buildSystemTestChangeMaterialization`
  — construct PSBT and metadata for test materializations.

## Relationships

- **Depends on**: `harness` (test environment), `darepod` (daemon under test).
- **Depended on by**: nothing (test-only).
