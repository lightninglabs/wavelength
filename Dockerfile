# Darepo Client (darepod) Multi-Stage Build
#
# Builds the darepod client daemon and darepocli tool from local source.
# The baselib/ submodule must be present since go.mod uses a local replace.
#
# Usage:
#   docker build -t darepod:local .
#   docker run darepod:local --network=regtest --wallet.type=lnd --lnd.host=lnd:10009

# --- Builder ---
FROM golang:1.26.0-alpine AS builder

RUN apk add --no-cache git

WORKDIR /build

# Copy dependency manifests first for layer caching. Include go.work so
# that workspace-only dependencies are also downloaded in this layer.
COPY go.mod go.sum go.work go.work.sum* ./
COPY baselib/go.mod baselib/go.sum ./baselib/
COPY tools/go.mod tools/go.sum ./tools/

RUN go mod download

# Copy full source (including baselib/ for replace directive).
COPY . .

# Build both binaries with CGO disabled for a static binary.
RUN CGO_ENABLED=0 go build -trimpath -o /out/darepod ./cmd/darepod
RUN CGO_ENABLED=0 go build -trimpath -o /out/darepocli ./cmd/darepocli

# --- Runtime ---
FROM alpine:3.21

RUN apk add --no-cache ca-certificates

COPY --from=builder /out/darepod /usr/local/bin/darepod
COPY --from=builder /out/darepocli /usr/local/bin/darepocli

# Daemon RPC port.
EXPOSE 10029

ENTRYPOINT ["darepod"]
