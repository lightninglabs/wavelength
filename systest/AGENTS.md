# systest

## Purpose

System-level end-to-end tests running the full operator daemon with real chain
backends and client connections.

## Key Types

- `InstrumentedMailbox` — Production-grade mailbox transport wrapper for tests.
  Replaces the old `BridgeClientConn` with real `ClientsConnBridge` wiring.
  Exposes `FlushFirstMatchingC2S(clientID, typeName)` to deliver the first
  buffered C2S envelope of the given friendly type, and
  `DropAllMatchingC2S(clientID, typeName)` to discard pre-crash buffered
  messages in restart scenarios.
- `MessageTranscript` — Records all server-to-client and client-to-server
  messages for test assertions.
- `TestClient` — In-process client wired with `serverconn.Runtime` and real OOR
  actor for full production transport testing. Key helpers:
  `OORReceiveRecipientOutput()` returns a recipient descriptor, while the
  `WithKey` variant also returns the signing key; `DisconnectForCrashRestart()`
  tears down the client bridge without stopping the database, enabling
  crash-restart tests.
  `NewTestClientWithExistingDBAndBridge` constructs a replacement client reusing
  an existing DB and bridge after a simulated crash.
- `RecipientQueryClient` — Standalone mailbox-backed indexer client used in
  systests to query `ListOORRecipientEventsByScript` independently from a
  running daemon. Useful for offline-recipient visibility tests.
- `serverconnBuilder` — Builds `serverconn` configurations for `TestClient`
  instances, including durable unary query support.
- `BatchSweeperRouter` — Routes batch sweeper messages in the systest harness.
- `VTXOObserver` — Tracks VTXO state changes during test execution.
- `WithShouldSeal(pred)` — Harness option injecting a `rounds.SealPredicate`
  for early round sealing tests.
- `WithRegistrationTimeout(d)` — Harness option overriding the registration
  timeout (used with seal predicates to prove the predicate fired, not the
  timer).
- `E2EHarness.CrashRestartClient(client)` — Stops a `TestClient` as a simulated
  crash and returns a new instance reusing the same DB and bridge.
- `E2EHarness.ServerVTXORecordStore()` — Returns the server's
  `*db.VTXORecordStoreDB` for direct state inspection in tests.

## Test Categories

- **Boarding** (`boarding_e2e_test.go`) — Full boarding round lifecycle.
- **OOR** (`oor_e2e_test.go`, `oor_package_test.go`) — End-to-end OOR transfers
  using production transport. Helper fixtures in `oor_vtxo_fixture_test.go` and
  `oor_realchain_helpers_test.go`.
- **Refresh** (`refresh_e2e_test.go`) — VTXO refresh lifecycle.
- **Seal Predicates** (`seal_predicate_e2e_test.go`) — Early round sealing.
- **Join Auth** (`join_auth_e2e_test.go`) — Client authentication during join.
- **Leave** (`leave_e2e_test.go`) — Client departure handling.

## Relationships

- **Depends on**: `harness` (test environment), `oor` (OOR actor and types),
  `clientconn` (production bridge), `serverconn` (client runtime), `rounds`,
  `batchsweeper`, most server packages.
- **Depended on by**: nothing (test-only).
