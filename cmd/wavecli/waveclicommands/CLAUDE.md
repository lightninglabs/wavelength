# waveclicommands

## Purpose

Importable cobra command definitions for the `wavecli` CLI. Separated from
the `wavecli` main package so test harnesses and other binaries can
embed the same command tree.

## CLI Surface

The CLI surface is organized into cobra command groups that shape the
default `--help` face:

1. **Wallet verbs (group "Wallet", implicit, no parent)** — the everyday
   commands that map 1:1 to what a user does day-to-day. All are
   wavewalletrpc-backed.
2. **Daemon introspection (group "Introspection")** — getinfo, schema, mcp
   (the built-in `help` command is grouped here too).
3. **Advanced subtrees (`ark`, `dev`, `recovery`)** — raw
   waverpc/devrpc commands for power users and operator runbooks.
   Hidden from the default `--help` via cobra `Hidden` (not a build tag),
   so they stay compiled and fully runnable in the shipped binary;
   `WAVELENGTH_DEV=1` reveals them under an "Advanced" group. The env var only
   changes visibility — it never gates execution. `ark.*` stays on the
   `schema`/MCP surfaces; `dev` is reachable only as the generated (hidden)
   CLI subtree — it is not registered in `schema`/MCP — and `recovery` is
   exposed on neither.

The `swap.*` verbs were retired: `send`/`recv --offchain` and `activity`
cover them (the swapruntime daemon runtime that powers those verbs is
unchanged).

### Top-level wallet verbs

| Command | RPC | Description |
|---------|-----|-------------|
| `create` | `wavewalletrpc.Create` | Initialize a new wallet (proxies GenSeed + InitWallet). Password from stdin / WAVED_WALLET_PASSWORD / --wallet_password_file |
| `unlock` | `wavewalletrpc.Unlock` | Unlock an existing wallet (proxies UnlockWallet) |
| `send <dest>` | `wavewalletrpc.Send` | Outbound payment. `--offchain` (default) for a BOLT-11 invoice via the swap subsystem; `--onchain` for an atomic on-chain send (`--sweep-all` drains). No prefix sniff |
| `recv` | `wavewalletrpc.Recv` / `wavewalletrpc.Deposit` | Inbound. `--offchain` (default) returns a Lightning invoice; `--onchain` returns a boarding address |
| `activity` | `wavewalletrpc.List` | Unified wallet activity view. Defaults to table output; `--format json` returns structured JSON. `--pending` and `--kind` narrow rows |
| `activity inspect <id>` | `wavewalletrpc.InspectActivity` | Correlated swap/VTXO/ledger detail for one activity entry |
| `balance` | `wavewalletrpc.Balance` | Flat balance (confirmed_sat, pending_in_sat, pending_out_sat) |
| `exit --outpoint TXID:VOUT` | `wavewalletrpc.Exit` | Queue a cooperative leave by default; unilateral unroll only fires with `--force-unroll-ack I_KNOW_WHAT_I_AM_DOING` |
| `exit status --outpoint TXID:VOUT` | `wavewalletrpc.ExitStatus` | Query an exit/unroll job's status (proxies GetUnrollStatus) |
| `exit summary` | `wavewalletrpc.ExitSummary` | Aggregate totals across all in-progress exits |
| `exit plan --outpoint ...` | `wavewalletrpc.GetExitPlan` | Preview backing-wallet funding readiness for one or more exits |
| `wallet-sweep --destination ADDR` | `wavewalletrpc.SweepWallet` | Preview, or with `--broadcast` publish, a sweep of confirmed backing-wallet UTXOs (boarding outputs excluded; see `ark sweep`) |

### Daemon introspection

| Command | RPC | Description |
|---------|-----|-------------|
| `getinfo` | `waverpc.GetInfo` | Daemon readiness, version, network, wallet state |
| `schema` | (local) | JSON method registry — single source of truth for CLI commands and MCP tools |
| `mcp` | (local) | MCP server exposing the schema as tools |

### `ark.*` advanced commands

The everyday wallet verbs compose `wavewalletrpc` end-to-end; the `ark`
parent surfaces the raw waverpc methods underlying them for callers
who want direct access.

| Command | RPC | Description |
|---------|-----|-------------|
| `ark vtxos {list,refresh,leave}` | `ListVTXOs` / `RefreshVTXOs` / `LeaveVTXOs` | VTXO inventory and lifecycle |
| `ark rounds {get,join,list,watch}` | `GetRound` / (join) / `ListRounds` / `WatchRounds` | Round FSM state; `join` commits queued intents into the next round (`vtxos refresh`/`leave` call it automatically) |
| `ark oor {receive,get,list}` | `NewReceiveScript` / `GetOORSession` / `ListOORSessions` | OOR session inspection |
| `ark board` | `Board` | Trigger boarding with confirmed UTXOs |
| `ark sweep [list]` | `SweepBoardingUTXOs` / `ListBoardingSweeps` | Boarding-timeout sweeps |
| `ark fees {estimate,history}` | `EstimateFee` / `GetFeeHistory` | Fee estimation and history |
| `ark listtransactions` | `ListTransactions` | Raw paginated transaction history |
| `ark send {inround,oor}` | `SendVTXO` / `SendOOR` | Raw in-round / OOR send (superseded by `send` for the wallet shape) |

### `recovery.*` advanced commands

Manual control of daemon-owned vHTLC recovery rows; normal swap clients
let the swap FSM arm and cancel recovery automatically.

| Command | RPC | Description |
|---------|-----|-------------|
| `recovery list` | `ListVHTLCRecoveries` | List recovery rows (ARMED rows are dormant) |
| `recovery status [id]` | `GetVHTLCRecoveryStatus` | Show one recovery row |
| `recovery escalate [id]` | `EscalateVHTLCRecovery` | Start on-chain unroll for an armed row; requires `--yes` on non-TTY stdin |
| `recovery cancel [id]` | `CancelVHTLCRecovery` | Record that cooperative settlement won; drop the armed row |

### `dev.*` advanced commands

Generated low-level daemon RPC CLI; see [`devrpc/`](devrpc/). Hidden from
the default `--help` (revealed with `WAVELENGTH_DEV=1`) but always runnable.

## Key Helpers

For field-level detail, use `go doc github.com/lightninglabs/wavelength/cmd/wavecli/waveclicommands.<Symbol>`.

- `NewRootCmd()` — top-level cobra command with all subcommands
  registered.
- `getDaemonConn()` / `getDaemonClient()` — TLS-by-default daemon
  gRPC dial; `--no-tls` opts out for local dev.
- `withWalletClient()` — maps `codes.Unimplemented` to
  `errWalletRPCDisabled` (with a pointer to `docs/wavewalletrpc_build.md`)
  for daemons built without the wavewalletrpc tag.
- `parseRequest()` — generic JSON-or-flags proto request parser
  (consumed by `ark.*` commands).
- `methodRegistry()` / `schemaMethod` / `schemaParam` —
  machine-readable schema for all CLI commands; shared source of
  truth for `schema` and MCP tool definitions. Built from the
  `walletAdmin`/`walletPayment`/`walletQuery`/`arkBase`/`arkVTXO`/
  `arkSend`/`arkObservable` sub-registries.
- `buildMCPServer()` — constructs the MCP server and registers every
  exposed RPC as a typed tool; split from `mcpServe` (which owns the
  daemon dial and stdio transport) so the tool surface is testable.
- `readPassword()` — reads wallet password from
  `WAVED_WALLET_PASSWORD` → `--wallet_password_file` → stdin → TTY.
  **Never from CLI args.**
- `validateDestination()` / `validateOutpoint()` /
  `validateFreeText()` — input hardening shared across the top-level
  wallet verbs (reject control chars, query/fragment chars, malformed
  outpoints, ambiguous flag combos).

## Relationships

- **Depends on**:
  - `rpc/wavewalletrpc` (generated stubs for the top-level wallet verbs;
    `WalletService` / `WalletInspectionService` clients).
  - `waverpc` (generated stubs for `ark.*`, `recovery.*`, getinfo).
  - `cmd/wavecli/waveclicommands/devrpc` (the generated `dev`
    subtree; its registry references `swapclientrpc` service
    descriptors).
- **Depended on by**: `cmd/wavecli` (main entry point).

## Invariants

- The top-level wallet verbs ALWAYS register at the root regardless
  of build tags; if the daemon lacks the wavewalletrpc tag, gRPC
  `Unimplemented` is mapped to `errWalletRPCDisabled` with an
  actionable message pointing at the build doc.
- The `--offchain` / `--onchain` flags on `send` and `recv` are
  mutually exclusive; if neither is set, offchain is the default. The
  CLI does NOT sniff the destination string — the daemon performs
  the authoritative parse.
- The wallet password is NEVER read from argv. The supported sources
  are `WAVED_WALLET_PASSWORD` (highest priority), then
  `--wallet_password_file`, then piped stdin, then interactive prompt.
- JSON output (`stdout`) and diagnostic output (`stderr`) are kept on
  separate streams so shell pipelines can consume the JSON body while
  a human reading the terminal sees informative warnings.
- `exit` defaults to a cooperative leave; it only starts a unilateral
  on-chain unroll when `--force-unroll-ack` matches the literal string
  `I_KNOW_WHAT_I_AM_DOING`, and that flag is mutually exclusive with
  `--onchain-address`.
- `recovery escalate` refuses to run on non-interactive stdin unless
  `--yes` is passed — it never blocks on a y/N prompt an agent can't
  answer.
- `ark vtxos refresh` is gated on fee consent: a real refresh fetches
  the dry-run estimate and prompts with it on a TTY, and refuses on
  non-interactive stdin without `--yes` (same posture as `leave --all`
  and `recovery escalate`). The MCP tool enforces the same contract
  through its `yes` argument — no prompt exists there, so a bare real
  refresh returns an immediate actionable error. `--dry_run` previews
  the itemized advisory estimate and never prompts. A failed estimate
  degrades to a "still charged the seal-time fee" warning and its
  total is absent on the wire (explicit proto presence) — it never
  blocks the flow and is never rendered as a zero fee.

## Deep Docs

- [docs/daemon_cli_guide.md](../../../docs/daemon_cli_guide.md) — Full CLI
  reference.
- [docs/wavewalletrpc_build.md](../../../docs/wavewalletrpc_build.md) — Build
  modes and what the wavewalletrpc tag enables.
- [rpc/wavewalletrpc/CLAUDE.md](../../../rpc/wavewalletrpc/CLAUDE.md) — Proto
  schema and per-message invariants.
- [swapwallet/CLAUDE.md](../../../swapwallet/CLAUDE.md) — Daemon-side
  implementation of the wallet verbs.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
