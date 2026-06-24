#!/bin/sh
# doc-check.sh — Verify documentation cross-links are valid.
#
# Checks:
#   1. All docs/ files referenced in CLAUDE.md exist.
#   2. All per-package CLAUDE.md files have an identical AGENTS.md.
#   3. All packages listed in ARCHITECTURE.md have a CLAUDE.md.
#   4. All files in docs/ are listed in docs/index.md.
#
# Uses only portable shell constructs (no bash-only features such as process
# substitution, `[[ ... ]]`, or `pipefail`). In particular we iterate with
# `for` over command substitution instead of piping into a `while` loop: a
# piped `while` runs in a subshell, so the `fail` helper's increments to
# `errors` would be lost and the script would always exit 0 regardless of the
# failures it printed. Scanned paths are repo-internal and whitespace-free, so
# word-splitting in the `for` lists is safe.

set -eu

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

errors=0

# fail reports an error without exiting immediately.
fail() {
	echo "ERROR: $1" >&2
	errors=$((errors + 1))
}

echo "==> Checking docs/ files referenced in CLAUDE.md..."
for docref in $(grep -oE 'docs/[a-zA-Z0-9_-]+\.md' CLAUDE.md | sort -u); do
	if [ ! -f "$docref" ]; then
		fail "CLAUDE.md references '$docref' but file does not exist"
	fi
done

echo "==> Checking per-package CLAUDE.md / AGENTS.md pairs..."
for claude_file in $(find . -mindepth 2 -name 'CLAUDE.md' \
	-not -path './.claude/*' -not -path './.git/*' \
	-not -path './vendor/*' | sort); do
	dir="$(dirname "$claude_file")"
	agents_file="$dir/AGENTS.md"
	if [ ! -f "$agents_file" ]; then
		fail "$claude_file exists but $agents_file is missing"
	elif ! diff -q "$claude_file" "$agents_file" > /dev/null 2>&1; then
		fail "$claude_file and $agents_file have diverged"
	fi
done

echo "==> Checking packages in ARCHITECTURE.md have CLAUDE.md..."
# Extract package directory references from markdown table links like
# [`name`](path/).
for pkg_path in $(grep -oE '\[`[^`]+`\]\([^)]+\)' ARCHITECTURE.md | \
	grep -oE '\([^)]+\)' | tr -d '()' | sed 's|/$||' | sort -u); do
	# Skip docs/ references, anchor-only links, and non-directory refs.
	case "$pkg_path" in
	docs/*|'#'*) continue ;;
	esac
	if [ -d "$pkg_path" ] && [ ! -f "$pkg_path/CLAUDE.md" ]; then
		fail "ARCHITECTURE.md references '$pkg_path/' but $pkg_path/CLAUDE.md is missing"
	fi
done

echo "==> Checking docs/ files are listed in docs/index.md..."
for docfile in $(find docs/ -maxdepth 1 -name '*.md' -not -name 'index.md' | \
	sort); do
	basename="$(basename "$docfile")"
	if ! grep -qF -- "$basename" docs/index.md; then
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
