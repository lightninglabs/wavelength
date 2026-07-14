# rpc

## Purpose

Client-side RPC message definitions and HTTP transport in sub-packages:

| Sub-package | Kind | Purpose |
|-------------|------|---------|
| `rpc/roundpb` | generated | Round protocol messages |
| `rpc/oorpb` | generated | OOR transfer messages |
| `rpc/swapclientrpc` | generated | Swap client service stubs |
| `rpc/wavewalletrpc` | generated | Highest-level wallet service stubs |
| `rpc/restclient` | hand-written | HTTP/protoJSON transport adapter |

## Relationships

- **Depends on**: nothing for generated sub-packages (proto definitions).
  `rpc/restclient` depends on `arkrpc`, `waverpc`, `mailbox/pb`,
  `rpc/swapclientrpc`, `rpc/wavewalletrpc`, and `swaprpc`.
- **Depended on by**: `round`, `oor`, `db` (roundpb/oorpb message types);
  `sdk/wavewalletdk`, `swapwallet`, `swapclientserver`, `waved`,
  `cmd/wavecli` (swapclientrpc/wavewalletrpc service stubs); `sdk/wavewalletdk`,
  `sdk/swaps`, `swapclientserver`, `waved` (`rpc/restclient` REST
  transport).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- `rpc/restclient` is the only hand-written sub-package here; all others
  are generated protobuf stubs.
