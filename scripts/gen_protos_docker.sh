#!/bin/bash

set -e

# This script uses Docker to compile the protobuf files. This ensures that
# everyone uses the same versions of protoc, protoc-gen-go, and other tools,
# regardless of their local development environment.

DIR="$(cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd)"
REPO_ROOT="$(cd "$DIR/.." && pwd)"
PARENT_ROOT="$(cd "$REPO_ROOT/.." && pwd)"
LND_SUBMODULE="$PARENT_ROOT/third_party/lnd"

# Extract dependency versions directly from go.mod instead of spinning up
# Docker containers. This avoids two ~10s docker run calls per invocation.
echo "Extracting dependency versions from go.mod..."
PROTOBUF_VERSION=$(grep -E '^\s+google\.golang\.org/protobuf\s+' "$REPO_ROOT/go.mod" \
  | awk '{print $2}')
: "${PROTOBUF_VERSION:?Could not extract protobuf version from go.mod}"
GRPC_GATEWAY_VERSION=$(grep -E '^\s+github\.com/grpc-ecosystem/grpc-gateway/v2\s+' "$REPO_ROOT/go.mod" \
  | awk '{print $2}')
: "${GRPC_GATEWAY_VERSION:?Could not extract grpc-gateway version from go.mod}"
GOOGLEAPIS_VERSION="${GOOGLEAPIS_VERSION:-v0.0.0-20260514144325-84009fb6ad89}"

echo "Building protobuf compiler docker image..."
docker build -t wavelength-protobuf-builder \
  --build-arg PROTOBUF_VERSION="$PROTOBUF_VERSION" \
  --build-arg GRPC_GATEWAY_VERSION="$GRPC_GATEWAY_VERSION" \
  --build-arg GOOGLEAPIS_VERSION="$GOOGLEAPIS_VERSION" \
  -f "$DIR/rpc.Dockerfile" \
  "$REPO_ROOT"

echo "Running proto compilation in docker..."
docker run \
  --rm \
  --user "$UID:$(id -g)" \
  -e COMPILE_MOBILE \
  -e GOPATH=/tmp/build/gopath \
  -v "$REPO_ROOT":/build \
  -v "$LND_SUBMODULE":/third_party/lnd \
  wavelength-protobuf-builder

echo "Proto compilation complete!"
