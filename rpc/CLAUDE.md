# rpc

## Purpose

Client-side RPC message definitions and HTTP transport in sub-packages:

| Sub-package | Kind | Purpose |
|-------------|------|---------|
| `rpc/roundpb` | generated | Round protocol messages |
| `rpc/oorpb` | generated | OOR transfer messages |
| `rpc/swapclientrpc` | generated | Swap client service stubs |
| `rpc/walletrpc` | generated | Highest-level wallet service stubs |
| `rpc/restclient` | hand-written | HTTP/protoJSON transport adapter |

## Relationships

- **Depends on**: nothing for generated sub-packages (proto definitions).
  `rpc/restclient` depends on `arkrpc`, `daemonrpc`, `mailbox/pb`,
  `rpc/swapclientrpc`, `rpc/walletrpc`, and `swaprpc`.
- **Depended on by**: `round`, `oor`, `serverconn` (generated types);
  `sdk/walletdk`, `cmd/darepocli` (`rpc/restclient` REST transport).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- `rpc/restclient` is the only hand-written sub-package here; all others
  are generated protobuf stubs.
