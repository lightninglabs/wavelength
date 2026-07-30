# Wavelength (waved) Multi-Stage Build
#
# Builds the waved client daemon and wavecli tool from local source.
# The baselib/ submodule must be present since go.mod uses a local replace.
#
# Usage:
#   docker build -t waved:local .
#   docker run waved:local --network=regtest --wallet.type=lnd --lnd.host=lnd:10009
#
# The default build uses the default tag set, which excludes the swap and
# wallet RPC subservers. To get the everyday wallet verbs (balance, send,
# recv, activity, ...) the daemon needs both optional tags:
#
#   docker build --build-arg GOTAGS="wavewalletrpc swapruntime" -t waved:wallet .

# --- Builder ---
FROM golang:1.26.0-alpine AS builder

# Optional Go build tags, taking the same space-separated value as the
# Makefile's tags= parameter (see build-wavewalletrpc). Empty by default so an
# unqualified `docker build` keeps producing the default tag set: no existing
# image silently grows a wallet RPC surface. Note that wavewalletrpc requires
# swapruntime alongside it; the pair is what the daemon checks.
ARG GOTAGS=""

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

# Build both binaries with CGO disabled for a static binary. Both get the same
# tag set, mirroring the Makefile's build target.
RUN CGO_ENABLED=0 go build -trimpath -tags="${GOTAGS}" -o /out/waved ./cmd/waved
RUN CGO_ENABLED=0 go build -trimpath -tags="${GOTAGS}" -o /out/wavecli ./cmd/wavecli

# --- Runtime ---
FROM alpine:3.21

RUN apk add --no-cache ca-certificates

COPY --from=builder /out/waved /usr/local/bin/waved
COPY --from=builder /out/wavecli /usr/local/bin/wavecli

# Daemon RPC port.
EXPOSE 10029

ENTRYPOINT ["waved"]
