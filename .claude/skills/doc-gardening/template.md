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
- **Messages to/from**: {actor message flows, if this package participates in
  the actor system. Format: "Sends X -> pkg, Receives Y <- pkg"}

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
- **Messages to/from**: Only include if the package participates in the actor
  system (sends/receives messages via Tell/Ask).
- **Invariants**: Focus on things that would cause bugs if violated. "Forfeit
  tx not broadcast until round confirms" is good. "Uses standard Go error
  handling" is not.
- **Deep Docs**: Link to existing docs/ files, README.md, and always link back
  to ARCHITECTURE.md.
