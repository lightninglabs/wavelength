# If you change this please also update GO_VERSION in the Makefile (then run
# `make lint` to see where else it needs to be updated as well).
FROM golang:1.26.0-bookworm

MAINTAINER Olaoluwa Osuntokun <laolu32@gmail.com>

# Golang build related environment variables that are static and used for all
# architectures/OSes.
ENV CGO_ENABLED=0

# Set up cache directories. Those will be mounted from the host system to
# speed up builds. If go isn't installed on the host system, those will fall
# back to temp directories during the build (see the docker-release target in
# the Makefile).
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
