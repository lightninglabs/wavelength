#!/bin/bash

set -e

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
	find . -name "*.proto" -print0 | xargs -0 clang-format --style=file:/build/scripts/.clang-format-proto -i

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

# generate_with_mailboxrpc compiles *.pb.go stubs including mailbox RPC
# service stubs via the protoc-gen-mailboxrpc plugin from the client
# submodule.
function generate_with_mailboxrpc() {
	local package=$1
	echo "Generating protos with mailboxrpc for ${package}"

	pushd "${package}" > /dev/null

	# Format proto files with clang-format using the proto-specific style.
	find . -name "*.proto" -print0 | xargs -0 clang-format --style=file:/build/scripts/.clang-format-proto -i

	# Generate the protos for each .proto file.
	for file in *.proto; do
		echo "  Generating ${file}"

		# Generate the standard Go protos.
		protoc -I/usr/local/include -I"${googleapis_include}" -I. -I.. \
			--go_out=. --go_opt=paths=source_relative \
			--go-grpc_out=. --go-grpc_opt=paths=source_relative \
			"${file}"

		# Generate RPC-over-mailbox stubs for service definitions.
		protoc -I/usr/local/include -I"${googleapis_include}" -I. -I.. \
			--mailboxrpc_out=. --mailboxrpc_opt=paths=source_relative \
			"${file}"
	done

	popd > /dev/null
}

# Generate protos for adminrpc (arkrpc lives in the client submodule
# and is handled by its own make rpc).
generate "adminrpc"

# Generate protos with mailbox RPC stubs for test packages that use
# the mailbox transport layer.
generate_with_mailboxrpc "clientconn/roundtestpb"

echo "Proto generation complete"
