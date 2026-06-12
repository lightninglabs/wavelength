# serverconn/hellotestpb

## Purpose

Test-only protobuf package providing a minimal `HelloService` RPC for
exercising `serverconn` mailbox infrastructure (routing, method dispatch,
crash recovery) without pulling in full round or OOR protocol logic.

## Key Types

- `HelloService` — Single RPC: `SayHello(HelloRequest) → HelloResponse`.
- `HelloRequest` — `Name string` field echoed back in the response.
- `HelloResponse` — `Message string` echo response.

## Relationships

- **Depends on**: nothing (pure proto-generated types).
- **Depended on by**: `serverconn` test files only; not imported by
  production code.

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- Test-only: must not be imported outside `serverconn` tests.

## Deep Docs

- [serverconn/CLAUDE.md](../CLAUDE.md) — Parent serverconn package overview.
