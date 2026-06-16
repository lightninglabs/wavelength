# rpc

## Purpose

Client-side RPC message definitions and HTTP transport in sub-packages:

| Sub-package | Kind | Purpose |
|-------------|------|---------|
| `rpc/roundpb` | generated | Round protocol messages |
| `rpc/oorpb` | mixed | OOR transfer messages (generated proto + handwritten payload helpers) |
| `rpc/swapclientrpc` | generated | Swap client service stubs |
| `rpc/walletdkrpc` | generated | Highest-level wallet service stubs |
| `rpc/restclient` | hand-written | HTTP/protoJSON transport adapter |

## Relationships

- **Depends on**: nothing for generated sub-packages (proto definitions).
  `rpc/restclient` depends on `arkrpc`, `daemonrpc`, `mailbox/pb`,
  `rpc/swapclientrpc`, `rpc/walletdkrpc`, and `swaprpc`.
- **Depended on by**: `round`, `oor`, `serverconn` (generated types);
  `sdk/walletdk`, `sdk/swaps`, `swapclientserver`, `darepod`
  (`rpc/restclient` REST transport).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- `rpc/restclient` and `rpc/oorpb` are the only sub-packages with
  hand-written Go; all other sub-packages are pure generated protobuf stubs.
