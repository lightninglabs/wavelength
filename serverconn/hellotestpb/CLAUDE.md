# serverconn/hellotestpb

## Purpose

Generated protobuf and mailbox-RPC stub code for a toy "hello world" service
(`hellotest.v1.HelloService`) used only to exercise `serverconn`'s unary
facade, durable event egress, and `EventRouter` dispatch in its own tests.
There is no hand-written Go in this package — both files are `DO NOT EDIT`
generated output. The source `.proto` lives outside this package, at
`serverconn/testdata/hello.proto`; running `make rpc` regenerates
`hello.pb.go` (via `protoc-gen-go`) and `hello_mailboxrpc.pb.go` (via the
in-repo `protoc-gen-mailboxrpc` plugin) from it.

## Key Types

- `HelloRequest` / `HelloResponse` — Unary request/response pair for
  `SayHello`, sent as a `KIND_REQUEST`/`KIND_RESPONSE` envelope pair via the
  unary facade.
- `GoodbyeRequest` / `GoodbyeResponse` — Unary request/response pair for
  `SayGoodbye`, same transport shape as `SayHello`.
- `JoinGreetingRequest` — Client-to-server fire-and-forget event
  (`KIND_EVENT`), dispatched via `SendClientEventRequest` through the durable
  actor mailbox; no response is expected.
- `HelloStartedEvent` — Server-to-client push notification (`KIND_EVENT`)
  simulating a server-initiated session start; routed to a client actor via
  an `EventRouter` dispatcher registered on
  `("hellotest.v1.HelloService", "HelloStarted")`.
- `HelloFinalizedEvent` — Server-to-client push notification (`KIND_EVENT`)
  simulating a session ending with a farewell message.
- `HelloServiceMailboxClient` / `HelloServiceMailboxServer` — Generated
  mailbox-RPC client and server interface for `HelloService`, plus
  `RegisterHelloServiceMailboxServer` for wiring an implementation into an
  `rpc.Router`.

## Relationships

- **Depends on**: `google.golang.org/protobuf` (generated message runtime),
  `mailbox/rpc` (`RPCClient`, `Router` — consumed only by the generated
  `hello_mailboxrpc.pb.go`).
- **Depended on by**: `serverconn` (test-only: `e2e_test.go`,
  `event_router_test.go` use these types to drive `Runtime`, `UnaryFacade`,
  and `EventRouter` against a fake in-memory edge).
- **Sends**:
  - → mailbox edge (via `serverconn`'s `UnaryFacade`): `HelloRequest`,
    `GoodbyeRequest` (unary requests); `JoinGreetingRequest` (durable event).
- **Receives**:
  - ← mailbox edge (via `serverconn`'s `EventRouter`/ingress): `HelloResponse`,
    `GoodbyeResponse` (unary responses); `HelloStartedEvent`,
    `HelloFinalizedEvent` (server-pushed events).

## Invariants

- Never hand-edit either `.go` file in this directory; regenerate via
  `make rpc` after editing `serverconn/testdata/hello.proto`.
- This package exists purely to give `serverconn`'s tests a concrete,
  generated service surface — it is not wired into `darepod` or any
  production RPC path. Do not add production dependencies on it.

## Deep Docs

- [docs/mailbox_architecture.md](../../docs/mailbox_architecture.md) —
  Describes the generated client/server stub shape this package is an
  example of (see "Generated Stubs and Code Generation").
- [serverconn/CLAUDE.md](../CLAUDE.md) — Parent package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
