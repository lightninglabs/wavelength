FROM golang:1.26.0-alpine

ENV GOFLAGS="-buildvcs=false"

RUN apk update && apk add --no-cache git && rm -rf /var/cache/apk/*

COPY . /tmp/tools

RUN cd /tmp/tools \
  && CGO_ENABLED=0 go install -trimpath github.com/golangci/golangci-lint/v2/cmd/golangci-lint \
  && CGO_ENABLED=0 golangci-lint custom \
  && mv ./custom-gcl /usr/local/bin/custom-gcl \
  && git config --global --add safe.directory /build \
  && rm -rf /go/pkg/mod \
  && rm -rf /root/.cache/go-build \
  && rm -rf /tmp/*

WORKDIR /build
