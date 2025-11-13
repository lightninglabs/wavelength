#!/bin/bash

set -e

# generate compiles the *.pb.go stubs from the *.proto files.
function generate() {
  # Generate the gRPC bindings for all proto files.
  for file in *.proto; do
    echo "Generating stubs for ${file}"
    protoc -I. -I.. \
      --go_out=. --go_opt=paths=source_relative \
      --go-grpc_out=. --go-grpc_opt=paths=source_relative \
      "${file}"
  done
}

# format formats the *.proto files with the clang-format utility.
function format() {
  if command -v clang-format >/dev/null 2>&1; then
    find . -name "*.proto" -print0 | xargs -0 clang-format --style=file -i
  else
    echo "clang-format not found, skipping formatting"
  fi
}

# Compile and format the adminrpc package.
format
generate
