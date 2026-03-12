#!/usr/bin/env bash
# doc-check.sh — Verify documentation cross-links are valid.
#
# Checks:
#   1. All docs/ files referenced in CLAUDE.md exist.
#   2. All per-package CLAUDE.md files have an identical AGENTS.md.
#   3. All packages listed in ARCHITECTURE.md have a CLAUDE.md.
#   4. All files in docs/ are listed in docs/index.md.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

ERRFILE=$(mktemp)
echo 0 > "$ERRFILE"

# Helper to report errors without exiting immediately.
fail() {
	echo "ERROR: $1" >&2
	count=$(cat "$ERRFILE")
	echo $((count + 1)) > "$ERRFILE"
}

echo "==> Checking docs/ files referenced in CLAUDE.md..."
for docref in $(grep -oE 'docs/[a-zA-Z0-9_-]+\.md' CLAUDE.md | sort -u); do
	if [ ! -f "$docref" ]; then
		fail "CLAUDE.md references '$docref' but file does not exist"
	fi
done

echo "==> Checking per-package CLAUDE.md / AGENTS.md pairs..."
while IFS= read -r claude_file; do
	dir="$(dirname "$claude_file")"
	agents_file="$dir/AGENTS.md"
	if [ ! -f "$agents_file" ]; then
		fail "$claude_file exists but $agents_file is missing"
	elif ! diff -q "$claude_file" "$agents_file" > /dev/null 2>&1; then
		fail "$claude_file and $agents_file have diverged"
	fi
done < <(find . -mindepth 2 -name 'CLAUDE.md' -not -path './.claude/*' \
	-not -path './.git/*' -not -path './vendor/*' -not -path './client/*' \
	| sort)

echo "==> Checking packages in ARCHITECTURE.md have CLAUDE.md..."
for pkg_path in $(grep -oE '\[`[^`]+`\]\([^)]+\)' ARCHITECTURE.md | \
	grep -oE '\([^)]+\)' | tr -d '()' | \
	sed 's|/$||' | sort -u); do
	# Skip docs/ references, anchor-only links, and non-directory refs.
	if [[ "$pkg_path" == docs/* ]] || [[ "$pkg_path" == \#* ]]; then
		continue
	fi
	if [ -d "$pkg_path" ] && [ ! -f "$pkg_path/CLAUDE.md" ]; then
		fail "ARCHITECTURE.md references '$pkg_path/' but $pkg_path/CLAUDE.md is missing"
	fi
done

echo "==> Checking docs/ files are listed in docs/index.md..."
while IFS= read -r docfile; do
	basename="$(basename "$docfile")"
	if ! grep -q "($basename)" docs/index.md 2>/dev/null; then
		fail "$docfile is not listed in docs/index.md"
	fi
done < <(find docs/ -maxdepth 1 -name '*.md' -not -name 'index.md' | sort)

errors=$(cat "$ERRFILE")
rm -f "$ERRFILE"

echo ""
if [ "$errors" -gt 0 ]; then
	echo "FAIL: $errors documentation cross-link error(s) found."
	exit 1
else
	echo "OK: All documentation cross-links are valid."
fi
