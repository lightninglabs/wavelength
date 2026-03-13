#!/bin/bash

set -e

# This script uses Docker to compile the protobuf files. This ensures that
# everyone uses the same versions of protoc, protoc-gen-go, and other tools,
# regardless of their local development environment.

DIR="$(cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd)"
REPO_ROOT="$(cd "$DIR/.." && pwd)"

# Extract dependency versions directly from go.mod instead of spinning up
# Docker containers. This avoids two ~10s docker run calls per invocation.
echo "Extracting dependency versions from go.mod..."
PROTOBUF_VERSION=$(grep -E '^\s+google\.golang\.org/protobuf\s+' "$REPO_ROOT/go.mod" \
  | awk '{print $2}')
GRPC_GATEWAY_VERSION=$(grep -E '^\s+github\.com/grpc-ecosystem/grpc-gateway/v2\s+' "$REPO_ROOT/go.mod" \
  | awk '{print $2}')

echo "Building protobuf compiler docker image..."
docker build -t darepo-protobuf-builder \
  --build-arg PROTOBUF_VERSION="$PROTOBUF_VERSION" \
  --build-arg GRPC_GATEWAY_VERSION="$GRPC_GATEWAY_VERSION" \
  -f "$DIR/rpc.Dockerfile" \
  "$REPO_ROOT"

echo "Running proto compilation in docker..."
docker run \
  --rm \
  --user "$UID:$(id -g)" \
  -e COMPILE_MOBILE \
  -v "$REPO_ROOT":/build \
  darepo-protobuf-builder

echo "Proto compilation complete!"
