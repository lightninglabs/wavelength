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
| [oor_subsystem.md](oor_subsystem.md) | The OOR (out-of-round) subsystem: per-session actor model, the `oor_session_registry` single source of truth, the Read/Commit turn with its inline outbox switch, outgoing/incoming flows, crash recovery, and the per-session concurrency fix for issue #605 |
| [fee_ledger.md](fee_ledger.md) | Client-side double-entry fee ledger: chart of accounts, per-flow walkthroughs, emission sites, replay safety |
| [fee-change-model.md](fee-change-model.md) | Seal-time fee handshake (#270): change-output designation rules, 11-scenario catalogue, proto contract, CLI mapping |
| [credit_system.md](credit_system.md) | Sat-native credit accounts for below-dust receives, credit-assisted receives, credit-backed sends, top-ups, and walletdk integration, with mermaid diagrams |
| [sdk_layered_architecture.md](sdk_layered_architecture.md) | SDK layering rationale: `sdk/ark` facade, remote vs. embedded modes, `sdk/swaps` future direction |
| [swap_system.md](swap_system.md) | End-to-end swap walkthrough: vHTLC tree (collaborative vs. unilateral-exit leaves), receive (out-swap) and pay (in-swap) flows, the off-chain-first cancellation/timeout recovery ladder, same-Ark p2p detection, swap-server RPCs, and proof-gated indexer authorization, with mermaid diagrams |
| [walletdk_integration.md](walletdk_integration.md) | Basic `walletdk` integration flow, startup/config examples, swap accounting, and wrapper guidance |
| [canonical_activity_log_design.md](canonical_activity_log_design.md) | Design (#774): a canonical store with stable cross-restart ids replacing the derive-on-read activity feed — a current-state `activity_entries` projection plus an append-only, sequence-numbered `activity_events` log (mailbox-style cursor) for resumable subscribe; foundation of the event-log epic (#776) |
| [walletdkrpc_build.md](walletdkrpc_build.md) | How to build and install the daemon and CLI with the wallet RPC subserver enabled (`walletdkrpc` + `swapruntime` tags) |
| [seed_restore_recovery.md](seed_restore_recovery.md) | Restore a self-managed seed and recover Ark state from chain/indexer data |
| [swap_background_execution.md](swap_background_execution.md) | Optional build-tagged daemon executor for background swap progress and daemon-backed CLI control |
| [mailbox_architecture.md](mailbox_architecture.md) | Three-layer mailbox system: pb, rpc, conn, serverconn |
| [mailbox_durable_actor_layer.md](mailbox_durable_actor_layer.md) | The durable mailbox and durable actor: leases, dedup, transactional outbox, DurableAsk, and the classic vs. Read/Commit (`TxBehavior`/`Exec[S]`) execution paths |
| [mailbox_transport_serverconn_clientconn.md](mailbox_transport_serverconn_clientconn.md) | The RPC-over-mailbox transport relating this client's `serverconn` to the operator's `clientconn`: envelope, edge API, shared `mailbox/conn` primitives, ack watermark, identity/auth/liveness, and the wire contract |
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
| [dev_rpc_cli_builder.md](dev_rpc_cli_builder.md) | Generated `darepocli dev` command builder and request flag rules |

## Release Engineering

| Document | Description |
|----------|-------------|
| [release_branch_management.md](release_branch_management.md) | Trunk-based release model: `v0.1.x-branch` release branches, the `main` `.99` version convention, tagging with `scripts/tag-release.sh` |
| [backport-workflow.md](backport-workflow.md) | Automated backport of merged `main` PRs to release branches via `backport-v*` labels |

## Operations

| Document | Description |
|----------|-------------|
| [daemon_cli_guide.md](daemon_cli_guide.md) | darepod/darepocli installation, configuration, CLI reference |
| [accounting_report.md](accounting_report.md) | Accounting report command: reading the fee ledger from SQLite or Postgres, text/JSON/CSV output, fiat conversion, read-only behavior |

## Plans

| Document | Description |
|----------|-------------|
| [PLANS.md](../PLANS.md) | ExecPlan specification for complex features |
