# Doc Gardening

Maintain the per-package documentation graph (`CLAUDE.md`/`AGENTS.md`) and
knowledge base (`docs/`, `ARCHITECTURE.md`) after code changes.

## Triggers

Use `/doc-gardening` when:
- A new package directory was created.
- Significant types, interfaces, or dependencies changed in existing packages.
- After a large refactor that moved or renamed packages.
- CI flags stale docs (e.g., `make doc-check` fails).

## Phase 1: Detect Changes

Determine what changed since the docs were last updated.

```bash
# Option A: Changes on current branch vs main.
git diff --name-only main...HEAD -- '*.go' | \
  sed 's|/[^/]*$||' | sort -u

# Option B: All packages missing CLAUDE.md.
find . -name '*.go' -not -path './.git/*' -not -path './vendor/*' | \
  sed 's|/[^/]*$||' | sort -u | while read dir; do
    [ ! -f "$dir/CLAUDE.md" ] && echo "MISSING: $dir"
  done
```

Classify each affected package into:
- **New** — has `.go` files but no `CLAUDE.md`.
- **Stale** — has `CLAUDE.md` but Go source changed since last update.

## Phase 2: Read Package Sources

For each affected package, gather:
1. `doc.go` (if exists) — package purpose.
2. Key exported types and interfaces (read primary `.go` files).
3. Import statements — which repo packages it imports.
4. `README.md` (if exists) — existing deep docs.

Use the Explore agent or direct Read/Grep for this. Be thorough — the
per-package docs should be accurate, not guessed.

## Phase 3: Generate or Update CLAUDE.md

Follow the standard template:

```markdown
# {package-name}

## Purpose
{1-2 sentences: what this package does}

## Key Types
- `TypeName` — {purpose}

## Relationships
- **Depends on**: pkg1, pkg2 (what it imports from this repo)
- **Depended on by**: pkg3, pkg4 (who imports it)
- **Messages to/from**: {actor message flows if applicable}

## Invariants
- {Critical invariants an agent must know}

## Deep Docs
- [link to relevant docs/ file if any]
- [link to README.md if exists]
```

**For new packages**: Create the file from scratch.
**For stale packages**: Read the existing `CLAUDE.md`, diff against current
source, and update only what changed. Do not rewrite content that is still
accurate.

## Phase 4: Mirror to AGENTS.md

After writing or updating each `CLAUDE.md`, copy it to `AGENTS.md` in the same
directory:

```bash
cp {package}/CLAUDE.md {package}/AGENTS.md
```

## Phase 5: Update ARCHITECTURE.md (if needed)

Check if:
- A new package needs to be added to the layer tables.
- Dependencies changed (package moved layers, new imports).
- A package was removed or renamed.

If so, update `ARCHITECTURE.md` accordingly.

## Phase 6: Update docs/index.md (if needed)

If new `docs/*.md` files were added, ensure they appear in `docs/index.md`
under the appropriate category.

## Phase 7: Validate

Run the cross-link checker:

```bash
make doc-check
```

Fix any errors before finishing.

## CI Integration

This skill can be invoked from CI in two modes:

**Check mode** (non-destructive, for PRs):
```bash
make doc-check
```
If this fails, the PR is flagged with a comment telling the author to run
`/doc-gardening` locally or let the CI agent fix it.

**Fix mode** (automated, for trusted branches):
The CI agent runs `/doc-gardening`, commits any updates, and pushes a fixup.

## Notes

- Do not update docs for generated code directories (`arkrpc`, `rpc`,
  `daemonrpc`) unless the proto definitions themselves changed.
- When in doubt about a type's purpose, use `go doc` to read its GoDoc.
- Prefer reading actual source over guessing from file names.
