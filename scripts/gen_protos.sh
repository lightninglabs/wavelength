#!/bin/bash

set -e

# generate compiles the *.pb.go stubs from the *.proto files.
function generate() {
	local package=$1
	echo "Generating protos for ${package}"

	pushd "${package}" > /dev/null

	# Format proto files with clang-format using the proto-specific style.
	find . -name "*.proto" -print0 | xargs -0 clang-format --style=file:/build/scripts/.clang-format-proto -i

	# Generate the protos for each .proto file.
	for file in *.proto; do
		echo "  Generating ${file}"

		# Generate the standard Go protos.
		protoc -I/usr/local/include -I. -I.. \
			--go_out=. --go_opt=paths=source_relative \
			--go-grpc_out=. --go-grpc_opt=paths=source_relative \
			"${file}"
	done

	# Generate the JSON/WASM client stubs using falafel for gomobile
	# compatibility. This is optional and only runs if COMPILE_MOBILE is set.
	if [ "$COMPILE_MOBILE" = "1" ]; then
		echo "  Generating mobile stubs with falafel"
		falafel=$(which falafel)
		opts="package_name=${package},js_stubs=1"
		protoc -I/usr/local/include -I. -I.. \
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
		protoc -I/usr/local/include -I. -I.. \
			--go_out=. --go_opt=paths=source_relative \
			--go-grpc_out=. --go-grpc_opt=paths=source_relative \
			"${file}"

		# Generate RPC-over-mailbox stubs for service definitions.
		protoc -I/usr/local/include -I. -I.. \
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
