# Documentation Index

Knowledge base for the darepo-server repository. Start with
[ARCHITECTURE.md](../ARCHITECTURE.md) for the system map, then follow links
into specific topics below.

## Architecture & Design

| Document | Description |
|----------|-------------|
| [ARCHITECTURE.md](../ARCHITECTURE.md) | Package layers, dependency graph, key types, patterns |
| [authoritative_locking.md](authoritative_locking.md) | Round and OOR locking model, ownership rules, and recovery invariants |
| [clientconn_architecture.md](clientconn_architecture.md) | Server-side 1:N mailbox connector architecture |
| [dispatch_pipeline.md](dispatch_pipeline.md) | Mailbox RPC dispatch pipeline (envelope, operator, actor) |
| [custom_scripting_state.md](custom_scripting_state.md) | Rolling note on custom-script VTXOs, checkpoint semantics, and open forfeit/auth work |

## Protocol Specifications

| Document | Description |
|----------|-------------|
| [ark-specs/ARK-00-overview.md](ark-specs/ARK-00-overview.md) | Protocol overview, terminology, and v1 status |
| [ark-specs/ARK-01-transactions.md](ark-specs/ARK-01-transactions.md) | Normative on-chain transaction formats (TRUC/P2A, Batch, VTXT, Forfeit, Sweep) |
| [ark-specs/ARK-02-rounds.md](ark-specs/ARK-02-rounds.md) | Round formation, seal-time fee handshake, MuSig2 signing ceremony |
| [ark-specs/ARK-03-oor.md](ark-specs/ARK-03-oor.md) | Out-of-round transfer protocol, checkpoint, and fraud-response ratchet |
| [ark-specs/ARK-04-monitoring.md](ark-specs/ARK-04-monitoring.md) | Operator-side on-chain monitoring and package-relay requirements |
| [ark-specs/ARK-05-client.md](ark-specs/ARK-05-client.md) | Client-side surface: RPCs, key derivation, unilateral exit |
| [ark-specs/ARK-06-wire-protocol.md](ark-specs/ARK-06-wire-protocol.md) | Wire protocol: mailbox, envelopes, gRPC service definitions |

## Development

| Document | Description |
|----------|-------------|
| [development_guidelines.md](development_guidelines.md) | Complete code style guide with WRONG/RIGHT examples |
| [ast-grep-guide.md](ast-grep-guide.md) | AST-level code search and lint rules |
| [structured-logging.md](structured-logging.md) | Structured log format, key-value helpers, error levels |
| [commit-tooling.md](commit-tooling.md) | commit_message.py workflows for linting and rewording |
| [testing-guide.md](testing-guide.md) | Coverage targets, test approaches, pre-commit checklist |

## Economics

| Document | Description |
|----------|-------------|
| [fee-model.md](fee-model.md) | Fee model spec: liquidity cost, on-chain share, congestion pricing, dust thresholds (companion: [fee-model-explorer.html](fee-model-explorer.html)) |
| [fee_ledger.md](fee_ledger.md) | Double-entry bookkeeping reference: chart of accounts, per-event (debit, credit) table, flow walkthroughs, replay-safety invariants |

## Testing

| Document | Description |
|----------|-------------|
| [layered_testing_guide.md](layered_testing_guide.md) | Test layering strategy and patterns |

## Plans

| Document | Description |
|----------|-------------|
| [PLANS.md](../PLANS.md) | ExecPlan specification for complex features |
