# darepoclicommands

## Purpose

Importable cobra command definitions for the `darepocli` CLI. Separated from
the `darepocli` main package so that test harnesses and other binaries can
embed the same command tree.

## Key Types

- `NewRootCmd()` — Creates the top-level cobra command with all subcommands.
- `getDaemonClient()` — Connects to the daemon's gRPC endpoint.
- `parseRequest()` — Generic JSON-or-flags request parser for proto messages.

## Commands

| Command | RPC | Description |
|---------|-----|-------------|
| `board` | `Board` | Trigger boarding with confirmed UTXOs |
| `rounds list` | `ListRounds` | List round FSM states with pagination |
| `rounds watch` | `WatchRounds` | Stream round state updates |
| `vtxos list` | `ListVTXOs` | List wallet VTXOs |
| `send` | `SendVTXO` / `SendOOR` | Send to address |
| `balance` | `GetBalance` | Show wallet balances |
| `oor receive` | `NewOORReceiveScript` | Register a new OOR receive script and print the receive address |
| `fees estimate` | `EstimateFee` | Print an itemized fee breakdown for a given VTXO amount; flags a dust-level amount on stderr |
| `fees history` | `GetFeeHistory` | Paginate the client-side ledger entries and print the cumulative operator fee total |
| `unroll` | `Unroll` | Trigger a unilateral exit for the VTXO at `--outpoint txid:index`. Routes through the VTXO manager's `ForceUnrollRequest` path so the FSM transitions cleanly; the registry job is created async via the chain resolver seam. Response includes `Created` (false if already exiting) and the `ActorId` to poll. |
| `unroll status` | `GetUnrollStatus` | Query progress for an unroll job by `--outpoint`. Reads through to the live registry first and falls back to the persisted `unilateral_exit_jobs` table for evicted/terminal jobs; `Found=false` (not an error) distinguishes "no such job" from lookup failure. |

## Relationships

- **Depends on**: `daemonrpc` (generated gRPC client stubs).
- **Depended on by**: `cmd/darepocli` (main entry point).

## Deep Docs

- [docs/daemon_cli_guide.md](../../../docs/daemon_cli_guide.md) — Full CLI reference.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
