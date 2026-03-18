#!/bin/sh

set -eu

dest="${1:?usage: local-custom-gcl.sh <dest>}"
script_dir=$(CDPATH='' cd -- "$(dirname "$0")" && pwd)

mkdir -p "$(dirname "$dest")"

if command -v custom-gcl >/dev/null 2>&1; then
	ln -sf "$(command -v custom-gcl)" "$dest"
	echo "Using custom-gcl from PATH."
	exit 0
fi

if [ -x "$dest" ]; then
	echo "Using local linter binary: $dest"
	exit 0
fi

if command -v go >/dev/null 2>&1; then
	if "$script_dir/install-custom-gcl.sh" "$dest"; then
		echo "Built native custom-gcl: $dest"
		exit 0
	fi

	cat >"$dest" <<'EOF'
#!/bin/sh

set -eu

repo_root=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
config_file="$repo_root/tools/.custom-gcl.yml"
gcl_version=""

if [ -f "$config_file" ]; then
	gcl_version=$(sed -n 's/^version:[[:space:]]*//p' "$config_file" | head -n 1)
fi

gcl_version=${gcl_version:-v1.64.5}

run_golangci() {
	exec go run "github.com/golangci/golangci-lint/cmd/golangci-lint@${gcl_version}" "$@"
}

if [ "${1:-}" = "run" ]; then
	shift

	tmpdir="$(mktemp -d "${TMPDIR:-/tmp}/custom-gcl.XXXXXX")"
	cfg="$tmpdir/custom-gcl.yml"
	trap 'rm -rf "$tmpdir"' EXIT

	awk '
		$0 ~ /^linters-settings:[[:space:]]*$/ {
			in_ls = 1
			print
			next
		}
		in_ls && $0 ~ /^  custom:[[:space:]]*$/ {
			skip_custom = 1
			next
		}
		skip_custom && $0 ~ /^  [A-Za-z0-9_-]+:[[:space:]]*$/ {
			skip_custom = 0
		}
		in_ls && $0 ~ /^[^[:space:]]/ {
			in_ls = 0
		}
		skip_custom {
			next
		}
		$0 ~ /^[[:space:]]*-[[:space:]]*ll[[:space:]]*$/ {
			sub(/ll/, "lll")
			print
			next
		}
		{
			print
		}
	' .golangci.yml >"$cfg"

	run_golangci run --config "$cfg" "$@"
fi

run_golangci "$@"
EOF

	chmod +x "$dest"
	echo "custom-gcl not found; using golangci-lint v1.64.5 fallback"
	echo "(custom linter plugin 'll' is disabled in local mode)."
	exit 0
fi

echo "error: install go or custom-gcl" >&2
exit 1
