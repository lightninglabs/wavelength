.PHONY: sqlc sqlc-check migrate-create migrate-up migrate-down gen
.PHONY: lint lint-source docker-tools fmt fmt-check tidy-module tidy-module-check
.PHONY: ast-lint ast-grep-fix
.PHONY: unit unit-cover unit-race check-go-version build install clean release
.PHONY: build rpc install help
.PHONY: submodule-init submodule-update submodule-status submodule-check submodule-sync
.PHONY: check-commits
.PHONY: systest systest-verbose

# Default target.
.DEFAULT_GOAL := build

# =========
# VARIABLES
# =========

PKG := github.com/lightninglabs/darepo
TOOLS_DIR := tools

GOCC ?= go

GOIMPORTS_PKG := github.com/rinchsan/gosimports/cmd/gosimports

GO_BIN := $(GOPATH)/bin
MIGRATE_BIN := $(GO_BIN)/migrate
GOIMPORTS_BIN := $(GO_BIN)/gosimports

# GO_VERSION is the Go version used for the release build, docker files, and
# GitHub Actions. This is the reference version for the project.
GO_VERSION := 1.25.3

GOBUILD := $(GOCC) build -v
GOINSTALL := $(GOCC) install -v
GOTEST := $(GOCC) test

GOFILES_NOVENDOR = $(shell find . -type f -name '*.go' -not -path "./vendor/*" -not -name "*pb.go" -not -name "*pb.gw.go" -not -name "*.pb.json.go" -not -path "./db/sqlc/*")

RM := rm -f
MAKE := make
XARGS := xargs -L 1

COMMIT := $(shell git describe --tags --dirty 2>/dev/null || echo "unknown")

# DB connection string for migrations (example).
DB_CONNECTIONSTRING ?= sqlite://./darepo.db

# Build tags.
DEV_TAGS := dev
RELEASE_TAGS :=

# Build flags for debug builds (similar to lnd).
DEV_GCFLAGS := -gcflags "all=-N -l"
DEV_LDFLAGS := -ldflags "-X $(PKG)/build.Commit=$(COMMIT)"

# Build flags for release builds.
RELEASE_LDFLAGS := -ldflags "-s -w -buildid= -X $(PKG)/build.Commit=$(COMMIT)"

ifneq ($(tags),)
DEV_TAGS += ${tags}
endif

# Coverage settings.
COVER_PKG = $$($(GOCC) list -deps -tags="$(DEV_TAGS)" ./... | grep '$(PKG)')
COVER_FLAGS = -coverprofile=coverage.txt -covermode=atomic -coverpkg=$(PKG)/...

# Include testing flags and variable definitions.
include make/testing_flags.mk

# Linting uses a lot of memory, so keep it under control by limiting the number
# of workers if requested.
ifneq ($(workers),)
LINT_WORKERS = --concurrency=$(workers)
endif

# Keep this in sync with run.build-tags in .golangci.yml.
LINT_BUILD_TAGS := test_postgres,test_sqlite

# Docker cache mounting strategy:
# - CI (GitHub Actions): Use bind mounts to host paths that GA caches persist.
# - Local: Use Docker named volumes (much faster on macOS/Windows due to
#   avoiding slow host-syncing overhead).
ifdef CI
# CI mode: bind mount to host paths that GitHub Actions caches.
DOCKER_TOOLS = docker run \
  --rm \
  -v $${HOME}/.cache/go-build:/root/.cache/go-build \
  -v $${HOME}/.cache/golangci-lint:/root/.cache/golangci-lint \
  -v $${HOME}/go/pkg/mod:/go/pkg/mod \
  -e GOPATH=/go \
  -v $$(pwd):/build darepo-tools
else
# Local mode: Docker named volumes for fast macOS/Windows performance.
DOCKER_TOOLS = docker run \
  --rm \
  -v darepo-go-build-cache:/root/.cache/go-build \
  -v darepo-go-lint-cache:/root/.cache/golangci-lint \
  -v darepo-go-mod-cache:/go/pkg/mod \
  -e GOPATH=/go \
  -v $$(pwd):/build darepo-tools
endif

GREEN := "\\033[0;32m"
NC := "\\033[0m"
define print
	@echo $(GREEN)$1$(NC)
endef

# Release build settings.
BUILD_SYSTEM := linux-amd64 linux-arm64 linux-armv7 darwin-amd64 darwin-arm64 windows-amd64

# By default we will build all systems. But with the 'sys' tag, a specific
# system can be specified. This is useful to release for a subset of
# systems/architectures.
ifneq ($(sys),)
BUILD_SYSTEM = $(sys)
endif

# ============
# DEPENDENCIES
# ============

$(GOIMPORTS_BIN):
	@$(call print, "Installing goimports.")
	cd $(TOOLS_DIR); $(GOCC) install -trimpath $(GOIMPORTS_PKG)

# Install golang-migrate if not present.
$(MIGRATE_BIN):
	@$(call print, "Installing golang-migrate")
	go install -tags 'postgres sqlite3' github.com/golang-migrate/migrate/v4/cmd/migrate@latest

# ============
# SQLC TARGETS
# ============

sqlc: #? Generate SQL code from schema and queries
	@$(call print, "Generating sql models and queries in Go")
	./scripts/gen_sqlc_docker.sh
	@$(call print, "Merging SQL migrations into consolidated schemas")
	go run ./cmd/merge-sql-schemas/main.go

sqlc-check: sqlc #? Verify SQL code generation is up to date
	@$(call print, "Verifying sql code generation")
	@if [ ! -f db/sqlc/schemas/generated_schema.sql ]; then \
		echo "Missing file: db/sqlc/schemas/generated_schema.sql"; \
		exit 1; \
	fi
	@if test -n "$$(git status --porcelain '*.go')"; then \
		echo "SQL models not properly generated!"; \
		git status --porcelain '*.go'; \
		exit 1; \
	fi

migrate-create: $(MIGRATE_BIN) #? Create a new migration (requires patchname=...)
	@$(call print, "Creating migration: $(patchname)")
	@if [ -z "$(patchname)" ]; then \
		echo "Error: patchname is required. Usage: make migrate-create patchname=add_new_table"; \
		exit 1; \
	fi
	migrate create -dir db/sqlc/migrations -seq -ext sql $(patchname)

migrate-up: $(MIGRATE_BIN) #? Apply all pending migrations
	@$(call print, "Applying all migrations")
	migrate -path db/sqlc/migrations -database $(DB_CONNECTIONSTRING) -verbose up

migrate-down: $(MIGRATE_BIN) #? Roll back one migration
	@$(call print, "Rolling back one migration")
	migrate -path db/sqlc/migrations -database $(DB_CONNECTIONSTRING) -verbose down 1

gen: sqlc rpc #? Generate all code (rpc, sqlc, etc.)

# ==============
# LINTING & CODE
# ==============

docker-tools:
	@$(call print, "Building tools docker image.")
	docker build -q -t darepo-tools $(TOOLS_DIR)

lint-source: docker-tools
	@$(call print, "Linting source.")
	$(DOCKER_TOOLS) custom-gcl run -v $(LINT_WORKERS)
	@$(call print, "Linting tag-guarded packages.")
	$(DOCKER_TOOLS) sh -ec '\
		base_pkgs=$$(mktemp); \
		tagged_pkgs=$$(mktemp); \
		guarded_tags=$$(mktemp); \
		trap "rm -f $$base_pkgs $$tagged_pkgs $$guarded_tags" EXIT; \
		go list ./... | sort -u > $$base_pkgs; \
		find . -name "*.go" \
			-not -path "./client/*" \
			-not -path "./vendor/*" \
			-not -path "./db/sqlc/*" \
			-exec sed -n "s#^//go:build[[:space:]][[:space:]]*##p" {} + | \
			tr "&|!()" "     " | tr "\t" " " | tr " " "\n" | \
			grep -E "^[A-Za-z_][A-Za-z0-9_]*$$" | sort -u > $$guarded_tags; \
		while IFS= read -r tag; do \
			[ -z "$$tag" ] && continue; \
			go list -tags "$$tag" ./... | sort -u > $$tagged_pkgs; \
			extra_pkgs=$$(comm -13 $$base_pkgs $$tagged_pkgs); \
			[ -z "$$extra_pkgs" ] && continue; \
			pkg_patterns=$$(printf "%s\n" "$$extra_pkgs" | \
				sed "s#^$(PKG)/#./#;s#^$(PKG)#.#"); \
			echo "Linting tag=$$tag for packages:"; \
			echo "$$pkg_patterns"; \
			custom-gcl run -v $(LINT_WORKERS) \
				--build-tags "$$tag,$(LINT_BUILD_TAGS)" \
				$$pkg_patterns; \
		done < $$guarded_tags'

lint: check-go-version lint-source #? Run static code analysis

# Globs to exclude generated files from ast-grep.
AST_GREP_EXCLUDE := --globs '!**/*.pb.go' --globs '!**/*.pb.gw.go' --globs '!**/*.pb.json.go' --globs '!**/db/sqlc/*.go'

# Optional directory/package filter for ast-grep (e.g., make ast-lint pkg=wallet).
AST_GREP_PATH := $(if $(pkg),$(pkg),.)

ast-lint: #? Run ast-grep style checks (requires ast-grep/sg installed). Use pkg=<dir> to focus on a specific directory.
	@$(call print, "Running ast-grep style checks.")
	sg scan $(AST_GREP_EXCLUDE) $(AST_GREP_PATH)

ast-grep-fix: #? Auto-fix ast-grep style issues (requires ast-grep/sg installed). Use pkg=<dir> to focus on a specific directory.
	@$(call print, "Auto-fixing ast-grep style issues.")
	sg scan --update-all $(AST_GREP_EXCLUDE) $(AST_GREP_PATH)

fmt: $(GOIMPORTS_BIN) #? Format code and fix imports
	@$(call print, "Fixing imports.")
	gosimports -w $(GOFILES_NOVENDOR)
	@$(call print, "Formatting source.")
	gofmt -l -w -s $(GOFILES_NOVENDOR)

fmt-check: fmt #? Verify code is formatted correctly
	@$(call print, "Checking fmt results.")
	if test -n "$$(git status --porcelain)"; then echo "code not formatted correctly, please run `make fmt` again!"; git status; git diff; exit 1; fi

tidy-module: #? Run 'go mod tidy' for all modules
	@$(call print, "Running 'go mod tidy' for all modules")
	cd $(TOOLS_DIR) && go mod tidy
	cd $(TOOLS_DIR)/linters && go mod tidy
	go mod tidy

tidy-module-check: tidy-module #? Verify modules are up to date
	if test -n "$$(git status --porcelain)"; then echo "modules not updated, please run `make tidy-module` again!"; git status; exit 1; fi

check-go-version: check-go-version-dockerfile check-go-version-yaml

check-go-version-dockerfile:
	@$(call print, "Checking for target Go version (v$(GO_VERSION)) in Dockerfile files")
	@./scripts/check-go-version.sh $(GO_VERSION) Dockerfile "FROM golang:"

check-go-version-yaml:
	@$(call print, "Checking for target Go version (v$(GO_VERSION)) in YAML files")
	@./scripts/check-go-version.sh $(GO_VERSION) "*.yml *.yaml" "go-version:\\|GO_VERSION:\\|go:"

# =======
# TESTING
# =======

unit: #? Run unit tests
	@$(call print, "Running unit tests.")
	$(UNIT)

unit-debug: #? Run unit tests with verbose output
	@$(call print, "Running unit tests with verbose output.")
	$(UNIT_DEBUG)

unit-cover: #? Run unit tests with coverage
	@$(call print, "Running unit coverage tests.")
	$(UNIT_COVER)

unit-race: #? Run unit tests with race detector
	@$(call print, "Running unit race tests.")
	env CGO_ENABLED=1 GORACE="history_size=7 halt_on_errors=1" $(UNIT_RACE)

check-commits: #? Run lint+unit on each commit since branch base (use upstream=<ref>, base=<ref>, keep_going=1, no_submodules=1)
	./scripts/check_commits_since_base.sh \
		$(if $(upstream),--upstream $(upstream),) \
		$(if $(base),--base $(base),) \
		$(if $(keep_going),--keep-going,) \
		$(if $(no_submodules),--no-submodules,)

# Database backend for systest: sqlite (default) or postgres.
# Usage: make systest db=postgres
SYSTEST_DB_TAG := $(if $(filter postgres,$(db)),test_postgres)
SYSTEST_TAGS := systest $(SYSTEST_DB_TAG)

# System tests are significantly heavier and can be flaky under high load on
# shared CI runners. Reduce parallelism in CI to make runs more stable.
ifdef CI
SYSTEST_PARALLEL ?= 2
endif

systest: #? Run system integration tests. Use db=postgres for PostgreSQL. Use case=TestName to run specific test.
	@$(call print, "Running system integration tests (db=$(or $(db),sqlite)).")
	env SYSTEST_PARALLEL="$(SYSTEST_PARALLEL)" $(GOTEST) \
		-tags "$(SYSTEST_TAGS)" -v ./systest/... -timeout 60m \
		$(if $(case),-run $(case),)

systest-verbose: #? Run system integration tests with verbose logging. Use db=postgres for PostgreSQL. Use case=TestName to run specific test.
	@$(call print, "Running system integration tests with verbose logging (db=$(or $(db),sqlite)).")
	env SYSTEST_PARALLEL="$(SYSTEST_PARALLEL)" $(GOTEST) \
		-tags "$(SYSTEST_TAGS)" -v ./systest/... -timeout 60m \
		-harness.logstdout $(if $(case),-run $(case),)

# ============
# RPC GENERATION
# ============

rpc: #? Generate RPC stubs from proto files (uses Docker)
	@$(call print, "Generating RPC stubs from proto files using Docker.")
	./scripts/gen_protos_docker.sh

# ============
# SUBMODULE MANAGEMENT
# ============

submodule-init: #? Initialize and update all submodules (first-time setup)
	@$(call print, "Initializing submodules.")
	./scripts/submodule_helper.sh init

submodule-update: #? Update submodules to latest remote commits
	@$(call print, "Updating submodules to latest commits.")
	./scripts/submodule_helper.sh update

submodule-status: #? Show detailed status of all submodules
	@$(call print, "Checking submodule status.")
	./scripts/submodule_helper.sh status

submodule-check: #? Verify submodules are initialized (CI check)
	@$(call print, "Verifying submodule status.")
	./scripts/submodule_helper.sh check

submodule-sync: #? Sync submodule URLs from .gitmodules
	@$(call print, "Syncing submodule URLs.")
	./scripts/submodule_helper.sh sync

# ============
# BUILDING
# ============

build: #? Build debug binaries and place in project directory
	@$(call print, "Building debug binaries.")
	$(GOBUILD) -trimpath -tags="$(DEV_TAGS)" $(DEV_GCFLAGS) $(DEV_LDFLAGS) -o . ./cmd/...

install: #? Build and install binaries to GOPATH/bin
	@$(call print, "Installing binaries.")
	$(GOINSTALL) -trimpath -tags="$(DEV_TAGS)" $(DEV_LDFLAGS) ./cmd/...

clean: #? Remove build artifacts
	@$(call print, "Cleaning build artifacts.")
	$(RM) ./merge-sql-schemas
	$(RM) -r ./bin

# ============
# INSTALLATION & RELEASE
# ============

release: #? Cross compile for all supported platforms
	@$(call print, "Cross compiling release binaries.")
	@mkdir -p ./bin
	@for sys in $(BUILD_SYSTEM); do \
		echo "Building for $$sys"; \
		export CGO_ENABLED=0 GOOS=$$(echo $$sys | cut -d- -f1) GOARCH=$$(echo $$sys | cut -d- -f2); \
		if [ "$$GOARCH" = "armv6" ]; then \
			export GOARCH=arm; export GOARM=6; \
		elif [ "$$GOARCH" = "armv7" ]; then \
			export GOARCH=arm; export GOARM=7; \
		fi; \
		$(GOBUILD) -trimpath $(RELEASE_LDFLAGS) -tags="$(RELEASE_TAGS)" -o ./bin/darepo-$$sys ./cmd/...; \
		echo; \
	done

# ============
# HELP
# ============

help: #? Show this help message
	@echo "Available make targets:"
	@echo ""
	@grep -h -E '^[a-zA-Z_-]+:.*?#\? .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?#\\? "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Examples:"
	@echo "  make build"
	@echo "  make rpc"
	@echo "  make unit"
	@echo "  make check-commits upstream=origin/main"
	@echo "  make unit pkg=db timeout=5m"
	@echo "  make unit-debug log=\"stdlog trace\" pkg=db case=TestFoo timeout=10s"
	@echo "  make unit tags=\"test_db_postgres\""
	@echo "  make systest"
	@echo "  make systest-verbose"
	@echo "  make systest db=postgres"
	@echo "  make migrate-create patchname=add_users_table"
