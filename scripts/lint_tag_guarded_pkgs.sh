#!/bin/sh

set -eu

: "${LINTER_BIN:?LINTER_BIN must be set}"
: "${LINT_BUILD_TAGS:?LINT_BUILD_TAGS must be set}"

base_pkgs="$(mktemp)"
tagged_pkgs="$(mktemp)"
guarded_tags="$(mktemp)"
trap 'rm -f "$base_pkgs" "$tagged_pkgs" "$guarded_tags"' EXIT

go list ./... | sort -u >"$base_pkgs"
find . -name "*.go" \
	-not -path "./client/*" \
	-not -path "./vendor/*" \
	-not -path "./db/sqlc/*" \
	-exec sed -n "s#^//go:build[[:space:]][[:space:]]*##p" {} + |
	tr "&|!()" "     " |
	tr "\t" " " |
	tr " " "\n" |
	grep -E "^[A-Za-z_][A-Za-z0-9_]*$" |
	sort -u >"$guarded_tags"

while IFS= read -r tag; do
	if [ -z "$tag" ]; then
		continue
	fi

	go list -tags "$tag" ./... | sort -u >"$tagged_pkgs"
	extra_pkgs="$(comm -13 "$base_pkgs" "$tagged_pkgs")"
	if [ -z "$extra_pkgs" ]; then
		continue
	fi

	pkg_patterns=""
	while IFS= read -r pkg; do
		if [ -z "$pkg" ]; then
			continue
		fi

		pkg_patterns="${pkg_patterns}${pkg_patterns:+
}${pkg}"
	done <<EOF
$(printf "%s\n" "$extra_pkgs" |
	sed \
		"s#^github.com/lightninglabs/darepo/#./#;s#^github.com/lightninglabs/darepo#.#")
EOF

	if [ -n "${LINT_BASE:-}" ]; then
		echo "Linting changed files for tag=$tag in packages:"
	else
		echo "Linting tag=$tag for packages:"
	fi
	printf "%s\n" "$pkg_patterns"

	set -- "$LINTER_BIN" run -v
	if [ -n "${LINT_TIMEOUT:-}" ]; then
		set -- "$@" "--timeout=${LINT_TIMEOUT}"
	fi
	if [ -n "${LINT_CONCURRENCY:-}" ]; then
		set -- "$@" "--concurrency=${LINT_CONCURRENCY}"
	fi
	set -- "$@" "--build-tags" "${tag},${LINT_BUILD_TAGS}"
	if [ -n "${LINT_BASE:-}" ]; then
		set -- "$@" "--new-from-merge-base=${LINT_BASE}"
	fi
	if [ "${LINT_WHOLE_FILES:-}" = "1" ]; then
		set -- "$@" "--whole-files"
	fi
	for pkg in $pkg_patterns; do
		set -- "$@" "$pkg"
	done

	"$@"
done <"$guarded_tags"
