# darepoclicommands

## Purpose

Importable cobra command definitions for the `darepocli` CLI. Separated from
the `darepocli` main package so test harnesses and other binaries can
embed the same command tree.

## CLI Surface

The CLI surface is split into three tiers:

1. **Top-level wallet verbs (implicit, no parent)** — the seven everyday
   commands that map 1:1 to what a user does day-to-day. All seven are
   walletrpc-backed.
2. **Daemon introspection at root** — getinfo, schema, mcp, dev.
3. **Advanced subtrees (`ark`, `swap`)** — raw daemonrpc/swapclientrpc
   commands for power users and operator runbooks.

### Top-level wallet verbs

| Command | RPC | Description |
|---------|-----|-------------|
| `create` | `walletrpc.Create` | Initialize a new wallet (proxies GenSeed + InitWallet). Password from stdin / DAREPOD_WALLET_PASSWORD / --wallet_password_file |
| `unlock` | `walletrpc.Unlock` | Unlock an existing wallet (proxies UnlockWallet) |
| `send <dest>` | `walletrpc.Send` | Outbound payment. `--offchain` (default) for Lightning invoice, `--onchain` for cooperative leave. No prefix sniff |
| `recv` | `walletrpc.Recv` / `walletrpc.Deposit` | Inbound. `--offchain` (default) returns a Lightning invoice; `--onchain` returns a boarding address |
| `list` | `walletrpc.List` | Unified wallet view. `--view {activity,vtxos,onchain}` selects the slice. `--pending` and `--kind` apply to activity |
| `balance` | `walletrpc.Balance` | Flat balance (confirmed_sat, pending_in_sat, pending_out_sat) |
| `exit --outpoint TXID:VOUT` | `walletrpc.Exit` | Trigger a unilateral exit (proxies Unroll) |
| `exit status --outpoint TXID:VOUT` | `walletrpc.ExitStatus` | Query an exit job's status (proxies GetUnrollStatus) |

### Daemon introspection

| Command | RPC | Description |
|---------|-----|-------------|
| `getinfo` | `daemonrpc.GetInfo` | Daemon readiness, version, network, wallet state |
| `schema` | (local) | JSON method registry — single source of truth for CLI commands and MCP tools |
| `mcp` | (local) | MCP server exposing the schema as tools |
| `dev *` | dev RPC | Generated dev RPC CLI (see `devrpc/`) |

### `ark.*` advanced commands

The everyday wallet verbs compose `walletrpc` end-to-end; the `ark`
parent surfaces the raw daemonrpc methods underlying them for callers
who want direct access.

All list-shaped `ark.*` verbs (`ark vtxos list`, `ark rounds list`,
`ark oor list`, `ark sweep list`, `ark listtransactions`) accept
`--fields <name1,name2,...>` to apply a field mask and `--ndjson` to
emit one JSON object per row on its own line (newline-delimited
JSON). Combine the two to shrink each line further. Both flags use
the shared `renderListOutput` helper so a behavior change in one
place flows to every list command.

`ark vtxos leave --all` no longer prompts for y/N when stdin is not
a terminal. An agent or pipeline must pass `--yes` (explicit
consent) or `--dry_run` (preview); otherwise the command fails fast
with an `INVALID_ARGS` envelope (exit code 2). When stdin is a TTY
the prompt still works for operator ergonomics. The TTY check is
exposed via the `stdinIsTTY` package variable so tests can pin both
branches deterministically.

| Command | RPC | Description |
|---------|-----|-------------|
| `ark vtxos {list,refresh,leave}` | `ListVTXOs` / `RefreshVTXOs` / `LeaveVTXOs` | VTXO inventory and lifecycle |
| `ark rounds {get,list,watch}` | `GetRound` / `ListRounds` / `WatchRounds` | Round FSM state |
| `ark oor {receive,get,list}` | `NewReceiveScript` / `GetOORSession` / `ListOORSessions` | OOR session inspection |
| `ark board` | `Board` | Trigger boarding with confirmed UTXOs |
| `ark sweep [list]` | `SweepBoardingUTXOs` / `ListBoardingSweeps` | Boarding-timeout sweeps |
| `ark fees {estimate,history}` | `EstimateFee` / `GetFeeHistory` | Fee estimation and history |
| `ark listtransactions` | `ListTransactions` | Raw paginated transaction history (superseded by `list --view onchain` for the wallet shape) |
| `ark send {inround,oor}` | `SendVTXO` / `SendOOR` | Raw in-round / OOR send (superseded by `send` for the wallet shape) |

### `swap.*` advanced commands (swapruntime build tag)

| Command | RPC | Description |
|---------|-----|-------------|
| `swap list` | `swapclientrpc.ListSwaps` | List persisted swap summaries |
| `swap show <hash>` | `GetSwap` | Fetch one swap by payment hash |
| `swap receive` | `StartReceive` | Create a receive swap |
| `swap pay` | `StartPay` | Pay a Lightning invoice from Ark funds |
| `swap resume` | `ResumeSwap` | Wake a persisted swap worker |
| `swap watch` | `SubscribeSwaps` | Stream swap summary updates |

Every swap.* verb accepts `--json` for raw proto payloads and is
registered as an MCP tool when the binary is built with
`-tags swapruntime`. The streaming `swap watch` verb is CLI-only;
MCP tool calls are request/response and a long-running subscription
fits the CLI lifecycle better. `validatePaymentHash` /
`validateInvoice` apply on both the CLI and MCP paths to catch the
canonical agent-hallucination patterns (whitespace, query suffixes,
wrong-length hashes, embedded control bytes) before the daemon's
decoder sees them.

Schema entries for swap.* are listed in `schema_registry_swap.go`
unconditionally regardless of build tag so a stub-build agent can
still discover the surface; the CLI itself prints a clear "rebuild
with -tags=swapruntime" error when the verb is invoked, and the
MCP registration is what actually gets tag-gated (`mcp_swap.go` vs
`mcp_swap_stub.go`).

## Key Types

- `NewRootCmd()` — Creates the top-level cobra command with all
  subcommands registered.
- `getDaemonConn()` / `getDaemonClient()` — Connect to the daemon's
  gRPC endpoint. TLS by default; `--no-tls` opts out for local
  development.
- `withWalletClient()` — Wraps a wallet RPC invocation; maps
  `codes.Unimplemented` from daemons that lack the walletrpc tag to a
  clear `errWalletRPCDisabled` error pointing at
  `docs/walletrpc_build.md`.
- `getSwapClient()` — Connects to the daemon's `swapclientrpc`
  subserver (`swapruntime` tag only).
- `parseRequest()` — Generic JSON-or-flags request parser for proto
  messages (consumed by `ark.*` commands).
- `methodRegistry()` / `schemaMethod` / `schemaParam` — Machine-readable
  schema for all CLI commands; shared source of truth for `schema` and
  MCP tool definitions. Built from `walletAdminMethodRegistry`,
  `walletPaymentMethodRegistry`, `walletQueryMethodRegistry`,
  `arkBaseMethodRegistry`, `arkVTXOMethodRegistry`, and
  `arkSendMethodRegistry` sub-registries.
- `readPassword()` — Reads the wallet password from
  DAREPOD_WALLET_PASSWORD / --wallet_password_file / stdin / TTY
  prompt. Never from CLI args.
- `validateDestination()` / `validateOutpoint()` /
  `validateFreeText()` — Input hardening shared across the seven
  top-level verbs. Reject control characters, query/fragment
  characters, malformed outpoints, ambiguous flag combos.

## Relationships

- **Depends on**:
  - `rpc/walletrpc` (generated stubs for the seven top-level verbs;
    `WalletService` client).
  - `daemonrpc` (generated stubs for `ark.*` commands and getinfo).
  - `rpc/swapclientrpc` (generated stubs for `swap.*` commands,
    `swapruntime` tag only).
- **Depended on by**: `cmd/darepocli` (main entry point).

## Agent-CLI Posture

This CLI is invoked routinely by AI agents, so the design follows the
agent-cli skill checklist (see `~/.claude/skills/agent-cli/SKILL.md`).
The relevant pieces here:

- **Machine-readable output by default**: every command emits
  proto-JSON on stdout; diagnostics go to stderr. Errors emit a
  structured envelope `{"error":{"code":..., "message":...}}` via
  `PrintError`.
- **Input hardening**: `validateDestination`, `validateOutpoint`,
  `validateFreeText`, `validateOutpointString`, and the per-verb
  flag-combination checks treat agent inputs as adversarial. They
  reject query / fragment characters in destinations, embedded
  control bytes in free-text fields, malformed outpoints, and
  ambiguous flag combinations before any RPC dispatch.
- **Dry-run safety rails for mutating verbs**: `send` and `exit`
  accept `--dry-run`, which runs every local validation and prints a
  `{"dry_run":true,"method":..., "body":...}` preview on stdout
  without dispatching the RPC. The process exits with code 10 on a
  passing dry-run so an agent can branch on it.
- **Semantic exit codes**: see `exit_codes.go`. 0 = success,
  2 = invalid args (rejected client-side), 3 = wallet/auth failure,
  4 = not found (unknown round / job / hash), 10 = dry-run passed,
  1 = anything else. gRPC status codes from the daemon also map onto
  this table via `ExitCodeFor`.
- **Schema introspection**: `schema` dumps the full method registry
  (top-level wallet verbs, ark.*, swap.*) as JSON. Each entry carries
  a `mcp_tool` flag indicating whether the method is also exposed
  over MCP.
- **MCP surface**: every read-only wallet verb (`balance`, `list`,
  `exit.status`) and every mutating wallet verb except `create` /
  `unlock` is registered as an MCP tool via `registerMCPWalletTools`.
  `create` / `unlock` are CLI-only by design: their inputs are
  secrets (wallet password, seed passphrase) that must not transit
  the MCP protocol where they could land in agent logs or provider
  APIs.

## Invariants

- The seven top-level wallet verbs ALWAYS register at the root
  regardless of build tags; if the daemon lacks the walletrpc tag,
  gRPC `Unimplemented` is mapped to `errWalletRPCDisabled` with an
  actionable message pointing at the build doc.
- The `--offchain` / `--onchain` flags on `send` and `recv` are
  mutually exclusive; if neither is set, offchain is the default. The
  CLI does NOT sniff the destination string — the daemon performs
  the authoritative parse.
- The wallet password is NEVER read from argv. The supported sources
  are `DAREPOD_WALLET_PASSWORD` (highest priority), then
  `--wallet_password_file`, then piped stdin, then interactive prompt.
- JSON output (`stdout`) and diagnostic output (`stderr`) are kept on
  separate streams so shell pipelines can consume the JSON body while
  a human reading the terminal sees informative warnings.
- The CLI and MCP surfaces share input-hardening builders
  (`buildWalletSendRequest`, `buildWalletListRequest`,
  `buildLeaveVTXOsRequest`, `buildOORRecipientOutput`,
  `parseDirectionField`). Divergence between the two surfaces would
  let one accept shapes the other rejects, so all flag-translation
  logic for tools that exist on both lives in shared helpers.

## Deep Docs

- [docs/daemon_cli_guide.md](../../../docs/daemon_cli_guide.md) — Full CLI
  reference.
- [docs/walletrpc_build.md](../../../docs/walletrpc_build.md) — Build
  modes and what the walletrpc tag enables.
- [rpc/walletrpc/CLAUDE.md](../../../rpc/walletrpc/CLAUDE.md) — Proto
  schema and per-message invariants.
- [swapwallet/CLAUDE.md](../../../swapwallet/CLAUDE.md) — Daemon-side
  implementation of the wallet verbs.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
