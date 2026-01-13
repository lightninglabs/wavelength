#!/usr/bin/env bash

set -euo pipefail

usage() {
	cat <<'EOF'
check_commits_since_base.sh

Checks each commit on the current branch (since the branch base) by running:
  - make lint
  - make unit

For each commit, prints its position and hash so you can later checkout and fix
the failing commit.

Usage:
  ./scripts/check_commits_since_base.sh [options]

Options:
  --upstream <ref>     Upstream ref to diff against (default: @{upstream},
                       else origin/main, origin/master, main, master).
  --base <ref>         Explicit base ref/commit (skips merge-base discovery).
  --keep-going         Keep checking commits after a failure.
  --no-submodules      Do not run 'git submodule update' after each checkout.
  -h, --help           Show this help.

Examples:
  ./scripts/check_commits_since_base.sh
  ./scripts/check_commits_since_base.sh --upstream origin/main
  ./scripts/check_commits_since_base.sh --keep-going
EOF
}

die() {
	echo "error: $*" >&2
	exit 1
}

have_clean_worktree() {
	test -z "$(git status --porcelain)"
}

resolve_upstream() {
	local upstream

	if upstream="$(git rev-parse --abbrev-ref --symbolic-full-name @{upstream} 2>/dev/null)"; then
		echo "$upstream"
		return 0
	fi

	if git show-ref --verify --quiet refs/remotes/origin/main; then
		echo "origin/main"
		return 0
	fi

	if git show-ref --verify --quiet refs/remotes/origin/master; then
		echo "origin/master"
		return 0
	fi

	if git show-ref --verify --quiet refs/heads/main; then
		echo "main"
		return 0
	fi

	if git show-ref --verify --quiet refs/heads/master; then
		echo "master"
		return 0
	fi

	return 1
}

run_make() {
	local target="$1"

	echo
	echo "--- make $target ---"
	# Avoid any local MAKEFLAGS interfering with output across commits.
	env -u MAKEFLAGS make "$target"
}

main() {
	local upstream_ref=""
	local base_ref=""
	local keep_going="false"
	local update_submodules="true"

	while [[ $# -gt 0 ]]; do
		case "$1" in
		--upstream)
			[[ $# -ge 2 && ${2:0:1} != '-' ]] || die "--upstream requires a value"
			upstream_ref="$2"
			shift 2
			;;
		--base)
			[[ $# -ge 2 && ${2:0:1} != '-' ]] || die "--base requires a value"
			base_ref="$2"
			shift 2
			;;
		--keep-going)
			keep_going="true"
			shift
			;;
		--no-submodules)
			update_submodules="false"
			shift
			;;
		-h | --help)
			usage
			exit 0
			;;
		*)
			die "unknown argument: $1 (use --help)"
			;;
		esac
	done

	git rev-parse --git-dir >/dev/null 2>&1 || die "not in a git repository"

	have_clean_worktree || die "worktree is dirty; commit or stash changes first"

	local orig_head
	orig_head="$(git rev-parse HEAD)"

	local orig_branch=""
	orig_branch="$(git symbolic-ref -q --short HEAD 2>/dev/null || true)"

	cleanup() {
		# Always restore the user's original checkout, even if we fail.
		if [[ -n "$orig_branch" ]]; then
			git checkout -q "$orig_branch" || true
		else
			git checkout -q "$orig_head" || true
		fi

		if [[ "$update_submodules" == "true" ]]; then
			git submodule update --init --recursive --quiet || true
		fi
	}
	trap cleanup EXIT INT TERM

	if [[ -z "$base_ref" ]]; then
		if [[ -z "$upstream_ref" ]]; then
			upstream_ref="$(resolve_upstream)" || die "could not infer upstream ref; use --upstream"
		fi

		base_ref="$(git merge-base HEAD "$upstream_ref")" ||
			die "could not find merge-base of HEAD and '$upstream_ref'"
	fi

	local base_short
	base_short="$(git rev-parse --short "$base_ref")"

	local commits_total
	commits_total="$(git rev-list --count "${base_ref}..HEAD")"

	echo "Upstream: ${upstream_ref:-"(none; base override)"}"
	echo "Base:     $base_short ($(git rev-parse "$base_ref"))"
	echo "Commits:  $commits_total"

	if [[ "$commits_total" -eq 0 ]]; then
		echo "No commits to check."
		return 0
	fi

	local i=0
	local failed="false"
	local failing_commit=""
	local failing_step=""

	while IFS= read -r commit; do
		i=$((i + 1))

		local short
		short="$(git rev-parse --short "$commit")"

		local subject
		subject="$(git log -1 --format=%s "$commit")"

		echo
		echo "=== [${i}/${commits_total}] ${short} ${subject} ==="

		git checkout -q "$commit"

		if [[ "$update_submodules" == "true" ]]; then
			git submodule update --init --recursive
		fi

		if ! run_make lint; then
			echo
			echo "FAIL [${i}/${commits_total}] $commit (lint)"
			failed="true"
			failing_commit="$commit"
			failing_step="lint"

			if [[ "$keep_going" != "true" ]]; then
				break
			fi
		fi

		if ! run_make unit; then
			echo
			echo "FAIL [${i}/${commits_total}] $commit (unit)"
			failed="true"
			failing_commit="$commit"
			failing_step="unit"

			if [[ "$keep_going" != "true" ]]; then
				break
			fi
		fi

		if [[ "$failed" != "true" ]]; then
			echo
			echo "OK [${i}/${commits_total}] $commit"
		fi
	done < <(git rev-list --reverse "${base_ref}..HEAD")

	if [[ "$failed" == "true" ]]; then
		echo
		echo "First failure: $failing_commit ($failing_step)"
		echo "To checkout that commit:"
		echo "  git checkout $failing_commit"
		return 1
	fi

	echo
	echo "All commits passed lint + unit."
}

main "$@"
