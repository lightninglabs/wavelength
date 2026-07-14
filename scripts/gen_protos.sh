#!/bin/bash

set -e

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${DIR}/.." && pwd)"

GOOGLEAPIS_VERSION="${GOOGLEAPIS_VERSION:-v0.0.0-20260514144325-84009fb6ad89}"
googleapis_include="$(go env GOMODCACHE)/github.com/googleapis/googleapis@${GOOGLEAPIS_VERSION}"
if [ ! -d "${googleapis_include}" ]; then
	go mod download "github.com/googleapis/googleapis@${GOOGLEAPIS_VERSION}"
fi

function check_gateway_config() {
	local proto_file=$1
	local gateway_config=$2

	for rpc in $(awk '/^[[:space:]]*rpc[[:space:]]+/{print $2}' "${proto_file}"); do
		if ! grep -Eq "selector:[[:space:]]+.*\\.${rpc}([[:space:]]|$)" "${gateway_config}"; then
			echo "RPC ${rpc} not added to ${gateway_config}"
			exit 1
		fi
	done
}

# generate compiles the *.pb.go stubs from the *.proto files.
function generate() {
	local package=$1
	local gateway=${2:-0}
	echo "Generating protos for ${package}"

	pushd "${package}" > /dev/null

	# Format proto files with clang-format using the proto-specific style.
	find . -name "*.proto" -print0 | xargs -0 clang-format \
		--style=file:"${REPO_ROOT}/scripts/.clang-format-proto" -i

	# Generate the protos for each .proto file.
	for file in *.proto; do
		echo "  Generating ${file}"

		# Generate the standard Go protos.
		protoc -I/usr/local/include -I"${googleapis_include}" -I. -I.. \
			--go_out=. --go_opt=paths=source_relative \
			--go-grpc_out=. --go-grpc_opt=paths=source_relative \
			"${file}"

		gateway_config="${file%.proto}.yaml"
		if [ "${gateway}" = "1" ] && [ -f "${gateway_config}" ]; then
			check_gateway_config "${file}" "${gateway_config}"
			protoc -I/usr/local/include -I"${googleapis_include}" -I. -I.. \
				--grpc-gateway_out=. \
				--grpc-gateway_opt=paths=source_relative \
				--grpc-gateway_opt=grpc_api_configuration="${gateway_config}" \
				"${file}"
		fi

		# Generate RPC-over-mailbox stubs for service definitions.
		#
		# The mailbox edge proto (mailboxpb) defines the underlying transport and
		# should not get an RPC-over-mailbox overlay, so we exclude it.
		if [ "${package}" != "mailbox/pb" ]; then
			protoc -I/usr/local/include -I"${googleapis_include}" -I. -I.. \
				--mailboxrpc_out=. --mailboxrpc_opt=paths=source_relative \
				"${file}"
		fi
	done

	# Generate the JSON/WASM client stubs using falafel for gomobile
	# compatibility. This is optional and only runs if COMPILE_MOBILE is set.
	if [ "$COMPILE_MOBILE" = "1" ]; then
		echo "  Generating mobile stubs with falafel"
		falafel=$(which falafel)
		opts="package_name=${package},js_stubs=1"
		protoc -I/usr/local/include -I"${googleapis_include}" -I. -I.. \
			--plugin=protoc-gen-custom=$falafel \
			--custom_out=. \
			--custom_opt="$opts" \
			$(find . -name '*.proto')
	fi

	popd > /dev/null
}

# Generate protos for mailbox edge transport and arkrpc.
generate "mailbox/pb" 1
generate "arkrpc" 1

# Generate round protocol protos for mailbox connector transport.
generate "rpc/roundpb"

# Generate optional daemon-owned swap client RPC protos.
generate "rpc/swapclientrpc" 1

# Generate optional daemon-owned simplified wallet RPC protos.
generate "rpc/walletdkrpc" 1

# Generate waverpc protos for the client daemon's own gRPC API.
generate "waverpc" 1

# Generate shared swap server RPC protos for the SDK and server.
generate "swaprpc" 1

# Generate OOR mailbox wire payload stubs.
generate "rpc/oorpb"

# Generate the low-level wavecli dev RPC command registry from the daemon
# and swap-client service descriptors.
go run ./cmd/wavecli/internal/gen-devrpc

# Generate adminrpc protos if present.
if [ -d "adminrpc" ]; then
	generate "adminrpc"
fi

echo "Proto generation complete"
