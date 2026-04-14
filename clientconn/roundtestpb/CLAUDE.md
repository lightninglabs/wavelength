# clientconn/roundtestpb

## Purpose

Generated protobuf stubs for the `RoundNotifyService` test RPC service used
exclusively in e2e tests to exercise the `clientconn` mailbox connector. Do
not edit manually; regenerate with `make rpc`.

## Key Types

- `RoundNotifyServiceServer` / `RoundNotifyServiceClient` — gRPC service
  interfaces for server-to-client round notifications.
- `RoundStartedNotification` / `RoundStartedAck` — Request/response pair for
  the `NotifyRoundStarted` unary RPC.
- `BatchReadyNotification` / `BatchReadyAck` — Request/response pair for the
  `NotifyBatchReady` unary RPC.
- `RoundStartedEvent` — Server-to-client push notification sent as a
  `KIND_EVENT` envelope when a new round starts.
- `ClientJoinedEvent` — Client-to-server fire-and-forget event sent as a
  `KIND_EVENT` envelope when a client joins a round.

## Relationships

- **Depends on**: nothing (generated protobuf code).
- **Depended on by**: `clientconn` tests / e2e harness (exercises the mailbox
  connector `UnaryFacade` and `EventRouter` dispatch paths).

## Invariants

- All `.pb.go` files are generated; only `round_test.proto` should be edited.
- Regenerate with `make rpc` after editing the `.proto` file.

## Deep Docs

- [docs/clientconn_architecture.md](../../docs/clientconn_architecture.md) — Full clientconn architecture.
- [docs/dispatch_pipeline.md](../../docs/dispatch_pipeline.md) — Envelope dispatch pipeline.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
