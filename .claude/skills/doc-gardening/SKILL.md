---
name: doc-gardening
description: >
  Maintain the per-package documentation graph (CLAUDE.md/AGENTS.md) and
  knowledge base (docs/, ARCHITECTURE.md) after code changes. Use when a new
  package was created, types/interfaces/dependencies changed significantly,
  after a large refactor, or when make doc-check fails.
argument-hint: "[package-path or 'all']"
allowed-tools: Read, Grep, Glob, Bash(make doc-check), Bash(go doc *), Bash(git diff *), Bash(find *), Bash(cp *), Bash(ls *)
---

# Doc Gardening

Maintain the per-package documentation graph after code changes. This skill
handles both **generating docs for new packages** and **updating stale docs**
for existing ones.

If `$ARGUMENTS` is a specific package path (e.g., `round` or `lib/tree`),
scope work to that package only. If `$ARGUMENTS` is `all` or empty, scan the
entire repo.

## Phase 1: Detect Changes

Determine what needs attention.

**For scoped runs** (specific package):
- Read the package's `.go` files and existing `CLAUDE.md` (if any).
- Determine if new or stale.

**For full runs** (`all` or no argument):

```bash
# Find packages missing CLAUDE.md
find . -name '*.go' -not -path './.git/*' -not -path './vendor/*' \
  -not -path './tools/*' | sed 's|/[^/]*$||' | sort -u | while read dir; do
    [ ! -f "$dir/CLAUDE.md" ] && echo "MISSING: $dir"
  done

# Find packages with Go changes on current branch
git diff --name-only main...HEAD -- '*.go' | sed 's|/[^/]*$||' | sort -u
```

Classify each affected package:
- **New** — has `.go` files but no `CLAUDE.md`.
- **Stale** — has `CLAUDE.md` but Go source changed since last update.

## Phase 2: Read Package Sources

For each affected package, gather:
1. `doc.go` (if exists) — package purpose.
2. Key exported types and interfaces (read primary `.go` files, use `go doc`).
3. Import statements — which repo packages it imports.
4. `README.md` (if exists) — existing deep docs to link.
5. **Actor message flows** — search for Tell/Ask calls, outbox message types,
   FSM event types, and `lib/actormsg` marker interfaces that cross package
   boundaries. List concrete Go type names for each send/receive direction.

Use the Explore agent for packages with many files, or direct Read/Grep for
small packages. Be thorough — per-package docs must be accurate, not guessed.

**Message tracing tips:**
- Search for `Tell(` and `Ask(` calls to find outbound messages.
- Search for types implementing `VTXOActorMsg`, `RoundReceivable`, or other
  `lib/actormsg` marker interfaces to find cross-boundary message types.
- Check FSM transition functions for outbox event types (emitted as side effects).
- Check `EventRouter` dispatch registrations in `serverconn` for ingress routing.

## Phase 3: Generate or Update CLAUDE.md

Follow the template in [template.md](template.md).

**For new packages**: Create from scratch using the template.
**For stale packages**: Read existing `CLAUDE.md`, diff against current source,
update only what changed. Do not rewrite content that is still accurate.

## Phase 4: Mirror to AGENTS.md

After writing or updating each `CLAUDE.md`, copy to `AGENTS.md`:

```bash
cp {package}/CLAUDE.md {package}/AGENTS.md
```

## Phase 5: Update ARCHITECTURE.md (if needed)

Check if:
- A new package needs to be added to the layer tables.
- Dependencies changed (package moved layers, new imports).
- A package was removed or renamed.

## Phase 6: Update docs/index.md (if needed)

If new `docs/*.md` files were added, ensure they appear in `docs/index.md`.

## Phase 7: Validate

```bash
make doc-check
```

Fix any errors before finishing. Report what was created/updated.

## Notes

- Skip generated code directories (`arkrpc`, `rpc`, `waverpc`) unless proto
  definitions changed.
- When in doubt about a type's purpose, run `go doc <package>.<Type>`.
- Prefer reading actual source over guessing from file names.
- For CI: `make doc-check` in check mode flags staleness; in fix mode the CI
  agent runs `/doc-gardening all` and commits updates.
