# Automated Backport Workflow

This document describes the automated backport workflow for wavelength. It
backports merged `main` pull requests to release branches (for example,
`v0.1.x-branch`) without manual cherry-picking.

## Overview

Instead of manually creating branches, cherry-picking commits, and opening PRs,
a maintainer adds a label to the merged `main` PR and the workflow does the rest:
it validates the target branch, cherry-picks the commits, and opens a PR against
the release branch.

## How to Use

1. Merge a PR to `main` (or start from one already merged).
2. Add a backport label in the format `backport-v<version>-branch`, for example
   `backport-v0.1.x-branch`.
3. The workflow then:
   - validates the target branch exists,
   - cherry-picks the commits,
   - opens a new PR targeting the release branch.

The label can be added before or after the merge — both trigger the workflow.
Adding it before merge fires on the PR-close event; adding it afterward fires on
the label event.

## Label Format

Labels **must** start with `backport-v` to trigger the workflow:

- ✅ `backport-v0.1.x-branch` → backports to `v0.1.x-branch`
- ✅ `backport-v0.2.x-branch` → backports to `v0.2.x-branch`

These labels are ignored:

- ❌ `backport candidate` — discussion label only
- ❌ `backport-candidate` — does not start with `backport-v`
- ❌ `needs-backport` — wrong prefix

This lets you use discussion labels without accidentally triggering a backport.
The branch name is everything after the `backport-` prefix:

```
Label:  backport-v0.1.x-branch
            ↓ (drop "backport-")
Branch: v0.1.x-branch
```

## Workflow Steps

1. **Checkout** — fetch full history and check out the PR's base branch
   (usually `main`).
2. **Validate target branches** — for each `backport-v*` label, extract the
   branch name and confirm it exists on `origin`. If a labeled branch is
   missing, that branch is skipped; the job only fails if every labeled branch
   is missing.
3. **Create backport PRs** — for each valid label, cherry-pick the PR's commits
   onto a fresh `backport-<pr-number>-to-<target-branch>` branch and open a PR
   against the release branch. Merge commits are skipped; authorship and commit
   messages are preserved. The backport PR title is
   `[<target-branch>] Backport #<pr-number>: <original title>` and its body links
   back to the original PR.

## Handling Conflicts

When the cherry-pick applies cleanly, the workflow opens a regular (non-draft)
PR ready for review. When it conflicts, the workflow commits the conflict markers
and opens a **draft** PR. Resolve it manually:

```bash
git fetch origin
git checkout backport-<pr-number>-to-v0.1.x-branch
# resolve conflicts in the affected files
git add <resolved-files>
git commit -m "Resolve backport conflicts"
git push origin backport-<pr-number>-to-v0.1.x-branch
# mark the PR "Ready for review" in the GitHub UI
```

Test locally before pushing, explain what conflicts were resolved in the commit
message, and get the backport PR reviewed like any other change.

## Multiple Backports

Add multiple `backport-v*` labels to backport to several release branches at
once. Each backport is processed independently: one may apply cleanly while
another conflicts, and each produces its own branch and PR.

## Technical Details

- **Workflow file:** `.github/workflows/backport.yml`
- **Triggers:** `pull_request_target` on `[closed, labeled]`
- **Condition:** runs only when the PR is actually merged and carries at least
  one label containing `backport-v`
- **Action:** [`korthout/backport-action`](https://github.com/korthout/backport-action),
  pinned to a commit hash, with `label_pattern: '^backport-(v.+)$'`,
  `merge_commits: skip`, and draft PRs on conflict

See [release_branch_management.md](release_branch_management.md) for how release
branches are created and tagged.
