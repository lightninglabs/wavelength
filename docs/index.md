# Documentation Index

Knowledge base for the darepo-server repository. Start with
[ARCHITECTURE.md](../ARCHITECTURE.md) for the system map, then follow links
into specific topics below.

## Architecture & Design

| Document | Description |
|----------|-------------|
| [ARCHITECTURE.md](../ARCHITECTURE.md) | Package layers, dependency graph, key types, patterns |
| [clientconn_architecture.md](clientconn_architecture.md) | Server-side 1:N mailbox connector architecture |
| [dispatch_pipeline.md](dispatch_pipeline.md) | Mailbox RPC dispatch pipeline (envelope, operator, actor) |

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

## Testing

| Document | Description |
|----------|-------------|
| [layered_testing_guide.md](layered_testing_guide.md) | Test layering strategy and patterns |

## Plans

| Document | Description |
|----------|-------------|
| [PLANS.md](../PLANS.md) | ExecPlan specification for complex features |
