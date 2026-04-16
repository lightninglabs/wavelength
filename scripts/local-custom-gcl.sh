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

gcl_version=${gcl_version:-v2.10.1}

run_golangci() {
	exec go run "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${gcl_version}" "$@"
}

if [ "${1:-}" = "run" ]; then
	shift

	tmpdir="$(mktemp -d "${TMPDIR:-/tmp}/custom-gcl.XXXXXX")"
	cfg="$tmpdir/custom-gcl.yml"
	trap 'rm -rf "$tmpdir"' EXIT

	awk '
		$0 ~ /^[[:space:]]*custom:[[:space:]]*$/ {
			custom_indent = match($0, /[^ ]/) - 1
			skip_custom = 1
			next
		}
		skip_custom {
			current_indent = match($0, /[^ ]/) - 1
			if ($0 ~ /^[[:space:]]*$/) {
				next
			}
			if (current_indent <= custom_indent) {
				skip_custom = 0
			} else {
				next
			}
		}
		skip_custom {
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
	echo "custom-gcl not found; using golangci-lint v2.10.1 fallback"
	echo "(custom linter plugin 'll' is disabled in local mode)."
	exit 0
fi

echo "error: install go or custom-gcl" >&2
exit 1
