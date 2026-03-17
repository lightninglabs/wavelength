#!/bin/sh

set -eu

dest="${1:?usage: local-custom-gcl.sh <dest>}"

mkdir -p "$(dirname "$dest")"

if command -v custom-gcl >/dev/null 2>&1; then
	ln -sf "$(command -v custom-gcl)" "$dest"
	echo "Using custom-gcl from PATH."
	exit 0
fi

if command -v go >/dev/null 2>&1; then
	cat >"$dest" <<'EOF'
#!/bin/sh

set -eu

run_golangci() {
	exec go run github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.5 "$@"
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

if [ -x "$dest" ]; then
	echo "Using local linter binary: $dest"
	exit 0
fi

echo "error: install go or custom-gcl" >&2
exit 1
