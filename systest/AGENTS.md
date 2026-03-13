# systest

## Purpose

System-level end-to-end tests running the full operator daemon with real chain
backends and client connections.

## Key Types

- `BridgeClientConn` — In-process bridge that replaces gRPC for server-to-client
  delivery in tests. Handles both rounds events (converted to client actor
  messages) and OOR responses (broadcast to `subscribe.Server` subscribers).
- `bridgeClientConnRef` — Lightweight `TellOnlyRef[ClientConnMsg]` adapter
  wrapping `BridgeClientConn` for injection into `oor.ActorCfg.ClientsConn`.
- `bridgeOOROutbox` — Bridge-based `OutboxHandler` that sends OOR requests via
  Tell and waits for async responses through the bridge subscription.
- `MessageTranscript` — Records all server-to-client and client-to-server
  messages for test assertions.
- `WithShouldSeal(pred)` — Harness option injecting a `rounds.SealPredicate` for early round sealing tests.
- `WithRegistrationTimeout(d)` — Harness option overriding the registration timeout (used with seal predicates to prove the predicate fired, not the timer).

## Relationships

- **Depends on**: `harness` (test environment), `oor` (OOR actor and types),
  `clientconn` (message types and ClientID), `rounds`, most server packages.
- **Depended on by**: nothing (test-only).
