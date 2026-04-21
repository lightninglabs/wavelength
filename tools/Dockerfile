FROM golang:1.25.5-alpine

ENV GOFLAGS="-buildvcs=false"

RUN apk update && apk add --no-cache git && rm -rf /var/cache/apk/*

COPY . /tmp/tools

RUN cd /tmp/tools \
  && version="$(sed -n 's/^version:[[:space:]]*//p' .custom-gcl.yml | head -n 1)" \
  && plugin_module="$(sed -n 's/^[[:space:]]*-[[:space:]]*module:[[:space:]]*//p' .custom-gcl.yml | head -n 1 | tr -d "'\"")" \
  && tmpdir="$(mktemp -d)" \
  && repo_dir="${tmpdir}/golangci-lint" \
  && GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 git clone \
    --branch "${version}" \
    --single-branch \
    --depth 1 \
    -c advice.detachedHead=false \
    -q \
    https://github.com/golangci/golangci-lint.git \
    "${repo_dir}" \
  && printf 'package main\n\nimport (\n\t_ "%s"\n)\n' "${plugin_module}" > "${repo_dir}/cmd/golangci-lint/plugins.go" \
  && cd "${repo_dir}" \
  && go mod edit -replace "${plugin_module}=/tmp/tools/linters" \
  && go mod tidy \
  && CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${version}-custom-gcl -X main.date=$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
    -o /usr/local/bin/custom-gcl \
    ./cmd/golangci-lint \
  && git config --global --add safe.directory /build \
  && rm -rf /go/pkg/mod \
  && rm -rf /root/.cache/go-build \
  && rm -rf /tmp/*

WORKDIR /build
