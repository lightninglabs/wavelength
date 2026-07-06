# Dockerfile for protobuf compilation. This ensures consistent proto generation
# across different development environments without requiring local protoc
# installations.
FROM golang:1.26.0-bookworm

RUN apt-get update && apt-get install -y \
  git \
  protobuf-compiler='3.21.12*' \
  clang-format='1:14.0*'

# We don't want any default values for these variables to make sure they're
# explicitly provided by parsing the go.mod file. Otherwise we might forget to
# update them here if we bump the versions.
ARG PROTOBUF_VERSION
ARG GRPC_GATEWAY_VERSION
ARG GOOGLEAPIS_VERSION

ENV PROTOC_GEN_GO_GRPC_VERSION="v1.5.1"
ENV FALAFEL_VERSION="v0.9.2"
ENV GOPATH=/tmp/build/gopath
ENV GOCACHE=/tmp/build/.cache
ENV GOMODCACHE=/tmp/build/.modcache
ENV PATH=/tmp/build/gopath/bin:$PATH

RUN cd /tmp \
  && mkdir -p /tmp/build/.cache \
  && mkdir -p /tmp/build/.modcache \
  && mkdir -p /tmp/build/gopath \
  && go mod download github.com/googleapis/googleapis@${GOOGLEAPIS_VERSION} \
  && go install google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOBUF_VERSION} \
  && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION} \
  && go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@${GRPC_GATEWAY_VERSION} \
  && go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2@${GRPC_GATEWAY_VERSION} \
  && go install github.com/lightninglabs/falafel@${FALAFEL_VERSION} \
  && go install golang.org/x/tools/cmd/goimports@v0.1.7 \
  && chmod -R 777 /tmp/build/

# Build the repo-local mailbox RPC protoc plugin into the image so it
# doesn't need to be compiled on every invocation. The .dockerignore
# limits the context to go.mod, go.sum, and the plugin source.
COPY go.mod go.sum /tmp/mailboxrpc/
COPY cmd/protoc-gen-mailboxrpc/ /tmp/mailboxrpc/cmd/protoc-gen-mailboxrpc/
RUN cd /tmp/mailboxrpc \
  && go install -buildvcs=false ./cmd/protoc-gen-mailboxrpc \
  && rm -rf /tmp/mailboxrpc

WORKDIR /build

CMD ["/bin/bash", "/build/scripts/gen_protos.sh"]
