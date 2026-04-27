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
| `fees estimate` | `EstimateFee` | Print an advisory fee estimate for a given VTXO amount; the binding fee is set by the server-issued `JoinRoundQuote` at seal time |
| `fees history` | `GetFeeHistory` | Paginate the client-side ledger entries and print the cumulative operator fee total |
| `unroll` | `Unroll` | Trigger a unilateral exit for the VTXO at `--outpoint txid:index`. Routes through the VTXO manager's `ForceUnrollRequest` path so the FSM transitions cleanly; the registry job is created async via the chain resolver seam. Response includes `Created` (false if already exiting) and the `ActorId` to poll. |
| `unroll status` | `GetUnrollStatus` | Query progress for an unroll job by `--outpoint`. Reads through to the live registry first and falls back to the persisted `unilateral_exit_jobs` table for evicted/terminal jobs; `Found=false` (not an error) distinguishes "no such job" from lookup failure. |
| `swap list` | (store-only) | List persisted Lightning swap sessions from the isolated `swaps.db`. `--pending` filters to resumable sessions; `--verbose` includes terminal reason and OOR session IDs. |
| `swap receive` | `sdk/swaps` | Create a Lightning invoice that deposits into Ark as a VTXO. Blocks until the vHTLC is funded and claimed. Requires `--amount` (sats). |
| `swap pay` | `sdk/swaps` | Pay a Lightning invoice from Ark VTXOs via vHTLC. Blocks until the payment completes or the session times out. Requires `--invoice`. |
| `swap resume` | `sdk/swaps` | Resume a persisted pay or receive session by payment hash. Detects direction from the store when `--direction` is omitted. |

The `swap` command group reads from and writes to an isolated SQLite database
(`--swapdb`, default `~/.darepod/swaps.db`) that is separate from the main
daemon database. All swap subcommands connect to both the Ark daemon
(`--rpcserver`) and the swap server (`--swapserver`).

## Relationships

- **Depends on**: `daemonrpc` (generated gRPC client stubs), `sdk/swaps`
  (swap client), `sdk/ark` (daemon connection adapter).
- **Depended on by**: `cmd/darepocli` (main entry point).

## Deep Docs

- [docs/daemon_cli_guide.md](../../../docs/daemon_cli_guide.md) — Full CLI reference.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
