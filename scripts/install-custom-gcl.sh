#!/bin/sh

set -eu

usage() {
	cat <<'EOF2'
usage: install-custom-gcl.sh <destination>

Build a native custom-gcl binary for the current host OS/ARCH using the
repo-local ll linter plugin and install it at <destination>.
EOF2
}

if [ "${1:-}" = "" ] || [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
	usage
	exit 1
fi

dest="$1"

if [ -d "$dest" ]; then
	echo "error: destination cannot be a directory: $dest" >&2
	exit 1
fi

script_dir=$(CDPATH='' cd -- "$(dirname "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)
tools_dir="$repo_root/tools"
config_file="$tools_dir/.custom-gcl.yml"
plugin_dir="$tools_dir/linters"

if ! command -v go >/dev/null 2>&1; then
	echo "error: go is required to build custom-gcl" >&2
	exit 1
fi

if ! command -v git >/dev/null 2>&1; then
	echo "error: git is required to build custom-gcl" >&2
	exit 1
fi

if [ ! -f "$config_file" ]; then
	echo "error: missing config file: $config_file" >&2
	exit 1
fi

if [ ! -d "$plugin_dir" ]; then
	echo "error: missing plugin module directory: $plugin_dir" >&2
	exit 1
fi

version=$(sed -n 's/^version:[[:space:]]*//p' "$config_file" | head -n 1)
plugin_module=$(
	sed -n 's/^[[:space:]]*-[[:space:]]*module:[[:space:]]*//p' \
		"$config_file" | head -n 1 | tr -d "'\""
)

if [ -z "$version" ]; then
	echo "error: unable to determine golangci-lint version" >&2
	exit 1
fi

if [ -z "$plugin_module" ]; then
	echo "error: unable to determine plugin module path" >&2
	exit 1
fi

tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/custom-gcl.XXXXXX")
trap 'rm -rf "$tmpdir"' EXIT HUP INT TERM

repo_dir="$tmpdir/golangci-lint"
tmp_bin="$tmpdir/custom-gcl"

echo "Building native custom-gcl ${version} for $(go env GOOS)/$(go env GOARCH)"
echo "Using plugin module: ${plugin_module}"

GIT_CONFIG_GLOBAL=/dev/null \
GIT_TERMINAL_PROMPT=0 \
git clone \
	--branch "$version" \
	--single-branch \
	--depth 1 \
	-c advice.detachedHead=false \
	-q \
	https://github.com/golangci/golangci-lint.git \
	"$repo_dir"

cat >"$repo_dir/cmd/golangci-lint/plugins.go" <<EOF2
package main

import (
	_ "${plugin_module}"
)
EOF2

(
	cd "$repo_dir"

	go mod edit -replace "${plugin_module}=${plugin_dir}"
	go mod tidy

	build_date=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
	ldflags="-s -w -X main.version=${version}-custom-gcl -X main.date=${build_date}"

	export GOFLAGS="${GOFLAGS-}${GOFLAGS:+ }-buildvcs=false"

	CGO_ENABLED=0 go build \
		-trimpath \
		-ldflags "$ldflags" \
		-o "$tmp_bin" \
		./cmd/golangci-lint
)

mkdir -p "$(dirname "$dest")"
mv "$tmp_bin" "$dest"
chmod +x "$dest"

echo "Installed native custom-gcl to: $dest"
