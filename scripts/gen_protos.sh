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

		# Generate RPC-over-mailbox stubs for service definitions.
		#
		# The mailbox edge proto (mailboxpb) defines the underlying transport and
		# should not get an RPC-over-mailbox overlay, so we exclude it.
		if [ "${package}" != "mailbox/pb" ]; then
			protoc -I/usr/local/include -I. -I.. \
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
		protoc -I/usr/local/include -I. -I.. \
			--plugin=protoc-gen-custom=$falafel \
			--custom_out=. \
			--custom_opt="$opts" \
			$(find . -name '*.proto')
	fi

	popd > /dev/null
}

# Generate protos for mailbox edge transport and arkrpc.
generate "mailbox/pb"
generate "arkrpc"

# Generate round protocol protos for mailbox connector transport.
generate "rpc/roundpb"

# Generate daemonrpc protos for the client daemon's own gRPC API.
generate "daemonrpc"

# Generate shared swap server RPC protos for the SDK and server.
generate "swaprpc"

# Generate OOR mailbox wire payload stubs.
generate "rpc/oorpb"

# Generate adminrpc protos if present.
if [ -d "adminrpc" ]; then
	generate "adminrpc"
fi

echo "Proto generation complete"
