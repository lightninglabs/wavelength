# Darepo Server (arkd) Multi-Stage Build
#
# Builds the arkd operator daemon and arkcli admin tool from local source.
# The client/ submodule must be present since go.mod uses a local replace.
#
# Usage:
#   docker build -t arkd:local .
#   docker run arkd:local --network=regtest --lnd.host=lnd:10009

# --- Builder ---
FROM golang:1.25.3-alpine AS builder

RUN apk add --no-cache git

WORKDIR /build

# Copy dependency manifests first for layer caching.
COPY go.mod go.sum ./
COPY client/go.mod client/go.sum ./client/
COPY client/baselib/go.mod client/baselib/go.sum ./client/baselib/

RUN go mod download

# Copy full source (including client/ submodule for replace directive).
COPY . .

# Build both binaries with CGO disabled for a static binary.
RUN CGO_ENABLED=0 go build -trimpath -o /out/arkd ./cmd/arkd
RUN CGO_ENABLED=0 go build -trimpath -o /out/arkcli ./cmd/arkcli

# --- Runtime ---
FROM alpine:3.21

RUN apk add --no-cache ca-certificates

COPY --from=builder /out/arkd /usr/local/bin/arkd
COPY --from=builder /out/arkcli /usr/local/bin/arkcli

# Client RPC and admin RPC ports.
EXPOSE 7070 8081

ENTRYPOINT ["arkd"]
