#!/usr/bin/env bash
# doc-check.sh — Verify documentation cross-links are valid.
#
# Checks:
#   1. All docs/ files referenced in CLAUDE.md exist.
#   2. All per-package CLAUDE.md files have a matching AGENTS.md.
#   3. All packages listed in ARCHITECTURE.md have a CLAUDE.md.
#   4. All files in docs/ are listed in docs/index.md.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

errors=0

# Helper to report errors without exiting immediately.
fail() {
	echo "ERROR: $1" >&2
	errors=$((errors + 1))
}

echo "==> Checking docs/ files referenced in CLAUDE.md..."
grep -oE 'docs/[a-zA-Z0-9_-]+\.md' CLAUDE.md | sort -u | while read -r docref; do
	if [ ! -f "$docref" ]; then
		fail "CLAUDE.md references '$docref' but file does not exist"
	fi
done

echo "==> Checking per-package CLAUDE.md / AGENTS.md pairs..."
find . -mindepth 2 -maxdepth 2 -name 'CLAUDE.md' -not -path './.claude/*' \
	-not -path './.git/*' | sort | while read -r claude_file; do
	dir="$(dirname "$claude_file")"
	agents_file="$dir/AGENTS.md"
	if [ ! -f "$agents_file" ]; then
		fail "$claude_file exists but $agents_file is missing"
	fi
done

echo "==> Checking packages in ARCHITECTURE.md have CLAUDE.md..."
# Extract package directory references from markdown table links like
# [`name`](path/).
grep -oE '\[`[^`]+`\]\([^)]+\)' ARCHITECTURE.md | \
	grep -oE '\([^)]+\)' | tr -d '()' | \
	sed 's|/$||' | sort -u | while read -r pkg_path; do
	# Skip docs/ references, anchor-only links, and non-directory refs.
	if [[ "$pkg_path" == docs/* ]] || [[ "$pkg_path" == \#* ]]; then
		continue
	fi
	if [ -d "$pkg_path" ] && [ ! -f "$pkg_path/CLAUDE.md" ]; then
		fail "ARCHITECTURE.md references '$pkg_path/' but $pkg_path/CLAUDE.md is missing"
	fi
done

echo "==> Checking docs/ files are listed in docs/index.md..."
find docs/ -maxdepth 1 -name '*.md' -not -name 'index.md' | sort | while read -r docfile; do
	basename="$(basename "$docfile")"
	if ! grep -q "$basename" docs/index.md 2>/dev/null; then
		fail "$docfile is not listed in docs/index.md"
	fi
done

echo ""
if [ "$errors" -gt 0 ]; then
	echo "FAIL: $errors documentation cross-link error(s) found."
	exit 1
else
	echo "OK: All documentation cross-links are valid."
fi
