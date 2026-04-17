# Documentation Index

Knowledge base for the darepo-client repository. Start with
[ARCHITECTURE.md](../ARCHITECTURE.md) for the system map, then follow links
into specific topics below.

## Architecture & Design

| Document | Description |
|----------|-------------|
| [ARCHITECTURE.md](../ARCHITECTURE.md) | Package layers, dependency graph, key types, patterns |
| [arkscript_spec.md](arkscript_spec.md) | RFC-style specification for the `lib/arkscript` tapscript policy system (AST, encoding, invariants, security considerations) |
| [durable_actor_architecture.md](durable_actor_architecture.md) | CDC pattern, durable mailbox lifecycle, recovery flow |
| [durable_actor_quickstart.md](durable_actor_quickstart.md) | Developer guide: TLVMessage, ActorBehavior, migration checklist |
| [fee_ledger.md](fee_ledger.md) | Client-side double-entry fee ledger: chart of accounts, per-flow walkthroughs, emission sites, replay safety |
| [mailbox_architecture.md](mailbox_architecture.md) | Three-layer mailbox system: pb, rpc, conn, serverconn |
| [RPC_MAILBOX_CONTRACT.md](RPC_MAILBOX_CONTRACT.md) | Envelope semantics, at-least-once delivery, ack watermarks |

## Development

| Document | Description |
|----------|-------------|
| [development_guidelines.md](development_guidelines.md) | Complete code style guide with WRONG/RIGHT examples |
| [ast-grep-guide.md](ast-grep-guide.md) | AST-level code search and lint rules |
| [structured-logging.md](structured-logging.md) | Structured log format, key-value helpers, error levels |
| [commit-tooling.md](commit-tooling.md) | commit_message.py workflows for linting and rewording |
| [testing-guide.md](testing-guide.md) | Coverage targets, test approaches, pre-commit checklist |
| [go_workspace.md](go_workspace.md) | Multi-module Go workspace setup |
| [policy_arkscript_review_guide.md](policy_arkscript_review_guide.md) | Policy-first arkscript reviewer guide |

## Operations

| Document | Description |
|----------|-------------|
| [daemon_cli_guide.md](daemon_cli_guide.md) | darepod/darepocli installation, configuration, CLI reference |

## Plans

| Document | Description |
|----------|-------------|
| [PLANS.md](../PLANS.md) | ExecPlan specification for complex features |
