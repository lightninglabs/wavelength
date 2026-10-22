# If you change this please also update GO_VERSION in the Makefile (then run
# `make lint` to see where else it needs to be updated as well).
FROM golang:1.27.3-bookworm@sha256:5c3e9b43cfd3bb95c7937462e29d87ada57b481a96fa23ad0eb61778d7a00110

LABEL maintainer="Olaoluwa Osuntokun <laolu32@gmail.com>"

# Golang build related environment variables that are static and used for all
# architectures/OSes.
ENV GODEBUG=netdns=cgo
ENV CGO_ENABLED=0

# Keep Go caches in the container so release cache cleanup cannot modify or
# race with the host's Go caches.
ENV GOCACHE=/tmp/build/.cache
ENV GOMODCACHE=/tmp/build/.modcache

RUN apt-get update && apt-get install -y \
    git \
    make \
    tar \
    zip \
    bash \
  && mkdir -p /tmp/build/wavelength \
  && mkdir -p /tmp/build/.cache \
  && mkdir -p /tmp/build/.modcache \
  && chmod -R 777 /tmp/build/

WORKDIR /tmp/build/wavelength
