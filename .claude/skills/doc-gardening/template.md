# Per-Package CLAUDE.md Template

Use this template when generating or updating per-package documentation files.

```markdown
# {package-name}

## Purpose

{1-2 sentences: what this package does, written as if explaining to someone
unfamiliar with the codebase.}

## Key Types

- `TypeName` — {one-line purpose}
- `InterfaceName` — {one-line purpose}

## Relationships

- **Depends on**: pkg1 (reason), pkg2 (reason)
- **Depended on by**: pkg3 (reason), pkg4 (reason)
- **Sends**:
  - → `target_pkg`: `ConcreteMessageType1`, `ConcreteMessageType2`
  - → `other_pkg`: `AnotherMessageType`
- **Receives**:
  - ← `source_pkg`: `InboundEventType1`, `InboundEventType2`
  - ← API: `UserFacingRequestType` (if applicable)

## Invariants

- {Critical invariant an agent must know when modifying this package}
- {Another invariant, if applicable}

## Deep Docs

- [link to relevant docs/ file](../docs/relevant-file.md) — description
- [README.md](README.md) — {if exists}
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
```

## Guidelines

- **Purpose**: Be specific. "Manages X" is better than "Provides X
  functionality."
- **Key Types**: List the top 3-5 exported types/interfaces. Skip internal
  types unless they are critical to understanding the package.
- **Relationships**: Use actual package names from this repo, not generic
  descriptions. Include the reason for the dependency.
- **Sends/Receives**: List concrete Go message type names, not descriptions.
  Use `→ pkg: TypeName` for sends and `← pkg: TypeName` for receives. Search
  for Tell/Ask calls, outbox message types, FSM event types, and
  `lib/actormsg` marker interfaces that cross package boundaries. Include
  API-facing requests (from RPC layer) under `← API:`. This section is
  critical — agents need to know what messages flow between subsystems to
  understand the actor wiring.
- **Invariants**: Focus on things that would cause bugs if violated. "Forfeit
  tx not broadcast until round confirms" is good. "Uses standard Go error
  handling" is not.
- **Deep Docs**: Link to existing docs/ files, README.md, and always link back
  to ARCHITECTURE.md.
