# serverconn/hellotestpb

## Purpose

Generated protobuf/mailbox-RPC stubs for `HelloService`, a test-only fixture
service used to exercise `serverconn`'s mailbox unary-RPC facade and event
router in `serverconn`'s own tests (`e2e_test.go`, `event_router_test.go`).
Proto source: `serverconn/testdata/hello.proto`. Not part of the production
API surface.

## Key Types

- `HelloServiceMailboxClient` — typed mailbox RPC client wrapping a
  `mailbox/rpc.RPCClient`; exposes `SayHello`/`SayGoodbye` as ordinary Go
  methods over `KIND_REQUEST`/`KIND_RESPONSE` envelope pairs.
- `HelloRequest`/`HelloResponse`, `GoodbyeRequest`/`GoodbyeResponse` — unary
  request/response messages for the two RPCs.
- `JoinGreetingRequest` — client-to-server fire-and-forget `KIND_EVENT`
  message (no response expected).
- `HelloStartedEvent` / `HelloFinalizedEvent` — server-to-client push
  notifications dispatched through `serverconn`'s `EventRouter` keyed on
  `"hellotest.v1.HelloService"` + method name (`HelloStarted`/
  `HelloFinalized`).

## Relationships

- **Depends on**: `mailbox/rpc` (mailbox-RPC runtime types consumed by the
  generated mailbox client).
- **Depended on by**: `serverconn` tests only (`e2e_test.go`,
  `event_router_test.go`); no non-test package imports this.

## Invariants

- **Never edit generated code** — both files are generated (`protoc-gen-go`
  for `hello.pb.go`, `protoc-gen-mailboxrpc` for `hello_mailboxrpc.pb.go`);
  edit `serverconn/testdata/hello.proto` instead. No make target or script
  covers this fixture; regenerate via a manual `protoc` invocation with
  `protoc-gen-go` and `protoc-gen-mailboxrpc`.
- This package exists solely to give `serverconn` tests a concrete service to
  drive; do not wire it into any production RPC surface.

## Deep Docs

- [docs/mailbox_architecture.md](../../docs/mailbox_architecture.md) —
  Mailbox RPC architecture; documents the `protoc-gen-mailboxrpc` output
  shape using `HelloService` as the worked example.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
