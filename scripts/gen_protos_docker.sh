#!/bin/bash

set -e

# This script uses Docker to compile the protobuf files. This ensures that
# everyone uses the same versions of protoc, protoc-gen-go, and other tools,
# regardless of their local development environment.

DIR="$(cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd)"
REPO_ROOT="$(cd "$DIR/.." && pwd)"

# Use golang image to extract versions from go.mod.
GO_IMAGE=docker.io/library/golang:1.25.3-alpine

echo "Extracting protobuf version from go.mod..."
PROTOBUF_VERSION=$(docker run --rm -v "$REPO_ROOT":/build -w /build $GO_IMAGE \
  go list -f '{{.Version}}' -m google.golang.org/protobuf)

echo "Extracting grpc-gateway version from go.mod..."
GRPC_GATEWAY_VERSION=$(docker run --rm -v "$REPO_ROOT":/build -w /build $GO_IMAGE \
  go list -f '{{.Version}}' -m github.com/grpc-ecosystem/grpc-gateway/v2)

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
