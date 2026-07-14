.PHONY: sqlc sqlc-check migrate-create migrate-up migrate-down gen
.PHONY: lint lint-source lint-local lint-source-local lint-changed-local lint-native build-native-linter local-custom-gcl install-custom-gcl docker-tools fmt fmt-changed fmt-check fmt-changed-check tidy-module tidy-module-check schema-check doc-check sample-conf-check
.PHONY: ast-lint ast-grep-fix
.PHONY: unit unit-cover unit-race unit-swapruntime check-go-version build install clean release
.PHONY: build build-swapruntime build-swapclient build-wavewalletrpc rpc install install-swapruntime install-wavewalletrpc help clean-networks
.PHONY: mobile mobile-android mobile-ios wasm-wallet
.PHONY: systest systest-verbose
.PHONY: commitmsg-lint commitmsg-fmt commitmsg-reword

# Default target.
.DEFAULT_GOAL := build

# =========
# VARIABLES
# =========

PKG := github.com/lightninglabs/wavelength
TOOLS_DIR := tools

GOCC ?= go

GOIMPORTS_PKG := github.com/rinchsan/gosimports/cmd/gosimports
GOLINT_PKG := github.com/golangci/golangci-lint/v2/cmd/golangci-lint
LLFORMAT_PKG := github.com/bhandras/llformat/cmd/llformat

GO_BIN := $(GOPATH)/bin
MIGRATE_BIN := $(GO_BIN)/migrate
GOIMPORTS_BIN := $(CURDIR)/$(TOOLS_DIR)/gosimports
LLFORMAT_BIN := $(CURDIR)/$(TOOLS_DIR)/llformat

# GO_VERSION is the Go version used for the release build, docker files, and
# GitHub Actions. This is the reference version for the project.
GO_VERSION := 1.26.0

GOBUILD := $(GOCC) build -v
GOINSTALL := $(GOCC) install -v
GOTEST := $(GOCC) test

GOFILES_NOVENDOR = $(shell find . -type f -name '*.go' -not -path "./vendor/*" -not -name "*pb.go" -not -name "*pb.gw.go" -not -name "*.pb.json.go" -not -path "./db/sqlc/*")

RM := rm -f
MAKE := make
XARGS := xargs -L 1

COMMIT := $(shell git describe --tags --dirty 2>/dev/null || echo "unknown")

# DB connection string for migrations (example).
DB_CONNECTIONSTRING ?= sqlite://./wavelength.db

# Build tags.
DEV_TAGS := dev
LOG_TAGS := nolog
TEST_FLAGS :=
RELEASE_TAGS :=

# Build flags for debug builds (similar to lnd).
DEV_GCFLAGS := -gcflags "all=-N -l"
DEV_LDFLAGS := -ldflags "-X $(PKG)/build.Commit=$(COMMIT)"

# Build flags for release builds.
RELEASE_LDFLAGS := -ldflags "-s -w -buildid= -X $(PKG)/build.Commit=$(COMMIT)"

ifneq ($(tags),)
DEV_TAGS += ${tags}
endif

# Logging tags - can be overridden with log= parameter.
# Examples: make unit log="stdlog trace"
# This enables stdout logging with trace level for debugging tests.
ifneq ($(log),)
LOG_TAGS := $(log)
endif

# Coverage settings.
COVER_PKG = $$($(GOCC) list -deps -tags="$(DEV_TAGS)" \
	-f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' \
	./... | grep '$(PKG)')
COVER_FLAGS = -coverprofile=coverage.txt -covermode=atomic -coverpkg=$(PKG)/...

# Default: list all packages for testing.
GOLIST := $(GOCC) list -tags="$(DEV_TAGS)" ./...

# If specific package is being unit tested, construct the full name of the
# subpackage and narrow GOLIST to just that package.
# NOTE: Submodules (e.g., baselib/) require go.work to resolve from root.
ifneq ($(pkg),)
UNITPKG := $(PKG)/$(pkg)
GOLIST := $(GOCC) list -tags="$(DEV_TAGS)" $(UNITPKG)
COVER_PKG = $(PKG)/$(pkg)
COVER_FLAGS = -coverprofile=coverage.txt -covermode=atomic -coverpkg=$(PKG)/$(pkg)
endif

# If a specific unit test case is being targeted, construct test.run filter.
ifneq ($(case),)
TEST_FLAGS += -test.run=$(case)
endif

# If a timeout is specified, add it to test flags.
ifneq ($(timeout),)
TEST_FLAGS += -timeout=$(timeout)
endif

# Test commands.
UNIT := $(GOLIST) | $(XARGS) env $(GOTEST) -tags="$(DEV_TAGS) $(LOG_TAGS)" $(TEST_FLAGS)
UNIT_RACE := $(UNIT) -race
UNIT_COVER := $(GOTEST) $(COVER_FLAGS) -tags="$(DEV_TAGS) $(LOG_TAGS)" $(TEST_FLAGS) $(COVER_PKG)

# Discover submodules (directories with go.mod, excluding tools/).
SUBMODULES := $(shell find . -mindepth 2 -name 'go.mod' -not -path './tools/*' -not -path './vendor/*' | xargs -I{} dirname {} | sed 's|^\./||')

# Linting uses a lot of memory, so keep it under control by limiting the number
# of workers if requested.
ifneq ($(workers),)
LINT_WORKERS = --concurrency=$(workers)
endif
LINT_BASE := $(if $(base),$(base),origin/main)
FMT_BASE := $(if $(base),$(base),origin/main)
LOCAL_CUSTOM_GCL := $(CURDIR)/$(TOOLS_DIR)/custom-gcl

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
  -v $$(pwd):/build wavelength-tools
else
# Local mode: Docker named volumes for fast macOS/Windows performance.
DOCKER_TOOLS = docker run \
  --rm \
  -v wavelength-go-build-cache:/root/.cache/go-build \
  -v wavelength-go-lint-cache:/root/.cache/golangci-lint \
  -v wavelength-go-mod-cache:/go/pkg/mod \
  -e GOPATH=/go \
  -v $$(pwd):/build wavelength-tools
endif

GREEN := \033[0;32m
NC := \033[0m
define print
	@printf '%b\n' '$(GREEN)$(subst ",,$1)$(NC)'
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

$(GOIMPORTS_BIN): $(TOOLS_DIR)/go.mod $(TOOLS_DIR)/go.sum
	@$(call print, "Installing goimports.")
	cd $(TOOLS_DIR); GOBIN="$(CURDIR)/$(TOOLS_DIR)" \
		$(GOCC) install -trimpath $(GOIMPORTS_PKG)

$(LLFORMAT_BIN): $(TOOLS_DIR)/go.mod $(TOOLS_DIR)/go.sum
	@$(call print, "Installing llformat.")
	cd $(TOOLS_DIR); GOBIN="$(CURDIR)/$(TOOLS_DIR)" \
		$(GOCC) install -trimpath $(LLFORMAT_PKG)

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
	docker build -q -t wavelength-tools $(TOOLS_DIR)

local-custom-gcl:
	@./scripts/local-custom-gcl.sh "$(LOCAL_CUSTOM_GCL)"

install-custom-gcl: #? Build and install a native custom-gcl binary to dest=<path> (default: ./tools/custom-gcl)
	@./scripts/install-custom-gcl.sh "$(if $(dest),$(dest),$(LOCAL_CUSTOM_GCL))"

lint-source: docker-tools
	@$(call print, "Linting source.")
	$(DOCKER_TOOLS) custom-gcl run -v --timeout=15m $(LINT_WORKERS)

lint-source-local: local-custom-gcl
	@$(call print, "Linting source locally (no Docker).")
	GOWORK=off $(LOCAL_CUSTOM_GCL) run -v --timeout=15m $(LINT_WORKERS)

lint-changed-local: local-custom-gcl #? Run static code analysis only for changes against base=<ref> locally (no Docker)
	@$(call print, "Linting source changes against $(LINT_BASE) locally.")
	GOWORK=off $(LOCAL_CUSTOM_GCL) run -v --timeout=15m $(LINT_WORKERS) \
		--new-from-merge-base=$(LINT_BASE) \
		--whole-files

build-native-linter: #? Build the custom golangci-lint binary natively via go tool
	@$(call print, "Building custom linter natively.")
	cd $(TOOLS_DIR) && CGO_ENABLED=0 $(GOCC) tool $(GOLINT_PKG) custom

lint-native: #? Deprecated alias for lint-local
	@$(call print, "lint-native is deprecated; use lint-local instead.")
	@$(MAKE) lint-local

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

lint: check-go-version check-migration-version lint-source #? Run static code analysis

lint-local: check-go-version check-migration-version lint-source-local #? Run static code analysis locally (no Docker)

fmt: $(GOIMPORTS_BIN) $(LLFORMAT_BIN) #? Format handwritten Go source and imports
	@$(call print, "Fixing imports for handwritten Go source.")
	@./scripts/llformat-files.sh all | \
		xargs -0 $(GOIMPORTS_BIN) -w
	@$(call print, "Formatting all handwritten Go source.")
	@./scripts/llformat-files.sh all | \
		xargs -0 $(LLFORMAT_BIN) -w

fmt-changed: $(GOIMPORTS_BIN) $(LLFORMAT_BIN) #? Format changed handwritten Go source and imports
	@$(call print, "Fixing imports for Go source changes against $(FMT_BASE).")
	@./scripts/llformat-files.sh changed "$(FMT_BASE)" | \
		xargs -0 -r $(GOIMPORTS_BIN) -w
	@$(call print, "Formatting Go source changes against $(FMT_BASE).")
	@./scripts/llformat-files.sh changed "$(FMT_BASE)" | \
		xargs -0 -r $(LLFORMAT_BIN) -w

fmt-check: fmt #? Verify code is formatted correctly
	@$(call print, "Checking fmt results.")
	if test -n "$$(git status --porcelain)"; then echo "code not formatted correctly, please run `make fmt` again!"; git status; git diff; exit 1; fi

fmt-changed-check: fmt-changed #? Verify changed Go source is formatted correctly
	@$(call print, "Checking changed fmt results.")
	if test -n "$$(git status --porcelain)"; then echo "changed code not formatted correctly, please run `make fmt-changed` again!"; git status; git diff; exit 1; fi

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

check-migration-version: #? Check that LatestMigrationVersion matches migration files
	@$(call print, "Checking migration version consistency.")
	@./scripts/check-migration-latest-version.sh

doc-check: #? Verify documentation cross-links are valid
	@$(call print, "Checking documentation cross-links.")
	@./scripts/doc-check.sh

sample-conf-check: #? Verify sample-waved.conf matches daemon config options
	@$(call print, "Checking sample-waved.conf.")
	$(GOCC) run ./scripts/check-sample-waved-conf

schema-check: #? Verify schema registry, MCP tools, and cobra commands are in sync
	@$(call print, "Verifying schema registry consistency.")
	$(GOCC) run scripts/verify-schema-registry/main.go

commitmsg-lint: #? Lint commit message(s). Use range=<rev-range>, commit=<rev>, or file=<path>
	@$(call print, "Linting commit message(s).")
	@if [ -n "$(range)" ]; then \
		./scripts/commit_message.py lint --range "$(range)"; \
	elif [ -n "$(commit)" ]; then \
		./scripts/commit_message.py lint --commit "$(commit)"; \
	elif [ -n "$(file)" ]; then \
		./scripts/commit_message.py lint --file "$(file)"; \
	else \
		./scripts/commit_message.py lint --commit HEAD; \
	fi

commitmsg-fmt: #? Format commit message. Use file=<path> [inplace=1] or commit=<rev>
	@$(call print, "Formatting commit message.")
	@if [ -n "$(file)" ]; then \
		if [ "$(inplace)" = "1" ]; then \
			./scripts/commit_message.py fmt --file "$(file)" --in-place \
				$(if $(filter 1,$(decode)),--decode-escaped-newlines,); \
		else \
			./scripts/commit_message.py fmt --file "$(file)" \
				$(if $(filter 1,$(decode)),--decode-escaped-newlines,); \
		fi; \
	elif [ -n "$(commit)" ]; then \
		./scripts/commit_message.py fmt --commit "$(commit)" \
			$(if $(filter 1,$(decode)),--decode-escaped-newlines,); \
	else \
		echo "Error: provide file=<path> or commit=<rev>"; \
		exit 1; \
	fi

commitmsg-reword: #? Reword a commit using formatted message. Use commit=<rev> (default HEAD)
	@$(call print, "Rewording commit with formatted message.")
	@./scripts/commit_message.py reword \
		--commit "$(if $(commit),$(commit),HEAD)" \
		$(if $(filter 1,$(decode)),--decode-escaped-newlines,) \
		$(if $(filter 1,$(dryrun)),--dry-run,)

# =======
# TESTING
# =======

unit: #? Run unit tests (root module and all submodules unless pkg= specified)
	@$(call print, "Running unit tests.")
	$(UNIT)
ifeq ($(pkg),)
	@$(call print, "Running submodule tests: $(SUBMODULES)")
	@for mod in $(SUBMODULES); do \
		printf '%b\n' '$(GREEN)>>> Testing submodule: '$$mod'$(NC)'; \
		(cd $$mod && $(GOTEST) -tags="$(DEV_TAGS) $(LOG_TAGS)" $(TEST_FLAGS) ./...) || { echo "FAILED: $$mod"; exit 1; }; \
	done
endif

unit-cover: #? Run unit tests with coverage
	@$(call print, "Running unit coverage tests.")
	$(UNIT_COVER)

unit-race: #? Run unit tests with race detector
	@$(call print, "Running unit race tests.")
	env CGO_ENABLED=1 GORACE="history_size=7 halt_on_errors=1" $(UNIT_RACE)

unit-swapruntime: #? Run unit tests with the optional wallet runtime enabled
	@$(call print, "Running unit tests with wallet runtime.")
	$(MAKE) unit tags="swapruntime wavewalletrpc"

# Database backend for systest: sqlite (default) or postgres.
# Usage: make systest db=postgres
SYSTEST_DB_TAG := $(if $(filter postgres,$(db)),test_postgres)
SYSTEST_TAGS := systest $(SYSTEST_DB_TAG)

# Per-package test timeout for systest. CI overrides this on ARC runners
# where the test harness boots slower than on local dev; locally 10m is
# typically plenty.
SYSTEST_TIMEOUT ?= 10m

systest: #? Run system integration tests. Use db=postgres for PostgreSQL.
	@$(call print, "Running system integration tests (db=$(or $(db),sqlite)).")
	$(GOTEST) -tags "$(SYSTEST_TAGS)" -v ./systest/... -timeout $(SYSTEST_TIMEOUT)

systest-verbose: #? Run system integration tests with verbose logging. Use db=postgres for PostgreSQL.
	@$(call print, "Running system integration tests with verbose logging (db=$(or $(db),sqlite)).")
	$(GOTEST) -tags "$(SYSTEST_TAGS)" -v ./systest/... -timeout $(SYSTEST_TIMEOUT) -harness.logstdout

# ============
# RPC GENERATION
# ============

rpc: #? Generate RPC stubs from proto files (uses Docker)
	@$(call print, "Generating RPC stubs from proto files using Docker.")
	./scripts/gen_protos_docker.sh

# ============
# BUILDING
# ============

build: #? Build debug binaries and place in project directory
	@$(call print, "Building debug binaries.")
	$(GOBUILD) -trimpath -tags="$(DEV_TAGS)" $(DEV_GCFLAGS) $(DEV_LDFLAGS) -o . ./cmd/merge-sql-schemas
	$(GOBUILD) -trimpath -tags="$(DEV_TAGS)" $(DEV_GCFLAGS) $(DEV_LDFLAGS) -o ./bin/waved ./cmd/waved
	$(GOBUILD) -trimpath -tags="$(DEV_TAGS)" $(DEV_GCFLAGS) $(DEV_LDFLAGS) -o ./bin/wavecli ./cmd/wavecli

build-swapruntime: #? Build debug binaries with SwapClientService enabled
	@$(call print, "Building debug binaries with swapruntime.")
	$(MAKE) build tags="swapruntime"

build-swapclient: build-swapruntime #? Alias for build-swapruntime

build-wavewalletrpc: #? Build debug binaries with wavewalletrpc + swapruntime enabled
	@$(call print, "Building debug binaries with wavewalletrpc and swapruntime.")
	$(MAKE) build tags="wavewalletrpc swapruntime"

mobile: #? Build gomobile bindings for sdk/wavewalletdk (target=android|ios|all)
	@$(call print, "Building gomobile wavewalletdk bindings.")
	./sdk/wavewalletdk/mobile/gen_bindings.sh $(or $(target),android)

mobile-android: #? Build the Android .aar for sdk/wavewalletdk
	@$(call print, "Building Android .aar for wavewalletdk.")
	./sdk/wavewalletdk/mobile/gen_bindings.sh android

mobile-ios: #? Build the iOS .xcframework for sdk/wavewalletdk
	@$(call print, "Building iOS .xcframework for wavewalletdk.")
	./sdk/wavewalletdk/mobile/gen_bindings.sh ios

WASM_WALLET_OUT := bin/wasm
WASMSQLITE_DIR := $(shell $(GOCC) list -m -f '{{.Dir}}' github.com/lightninglabs/go-wasmsqlite 2>/dev/null)

wasm-wallet: #? Build the wavewalletdk browser wasm blob + runtime assets into bin/wasm
	@$(call print, "Building wavewalletdk browser wasm blob.")
	$(RM) -r $(WASM_WALLET_OUT)
	mkdir -p $(WASM_WALLET_OUT)
	GOOS=js GOARCH=wasm $(GOBUILD) -trimpath -ldflags="-s -w" \
		-tags="mobile wavewalletrpc swapruntime" \
		-o $(WASM_WALLET_OUT)/wavewalletdk.wasm ./cmd/wavewalletdk-wasm
	gzip -9 -c $(WASM_WALLET_OUT)/wavewalletdk.wasm \
		> $(WASM_WALLET_OUT)/wavewalletdk.wasm.gz
	cp "$$($(GOCC) env GOROOT)/lib/wasm/wasm_exec.js" $(WASM_WALLET_OUT)/
	cp $(WASMSQLITE_DIR)/assets/sqlite3.js $(WASM_WALLET_OUT)/
	cp $(WASMSQLITE_DIR)/assets/sqlite3.wasm $(WASM_WALLET_OUT)/
	cp $(WASMSQLITE_DIR)/assets/sqlite3-opfs-async-proxy.js $(WASM_WALLET_OUT)/
	cp $(WASMSQLITE_DIR)/bridge/sqlite-bridge.js $(WASM_WALLET_OUT)/
	cp $(WASMSQLITE_DIR)/bridge/sqlite-worker.js $(WASM_WALLET_OUT)/
	# The go-wasmsqlite assets are read-only in the module cache; make the
	# copies writable so re-runs (and callers staging the bundle) aren't
	# blocked by a read-only destination.
	chmod -R u+w $(WASM_WALLET_OUT)

install: #? Build and install binaries to GOPATH/bin
	@$(call print, "Installing binaries.")
	$(GOINSTALL) -trimpath -tags="$(DEV_TAGS)" $(DEV_LDFLAGS) ./cmd/merge-sql-schemas
	$(GOINSTALL) -trimpath -tags="$(DEV_TAGS)" $(DEV_LDFLAGS) ./cmd/waved
	$(GOINSTALL) -trimpath -tags="$(DEV_TAGS)" $(DEV_LDFLAGS) ./cmd/wavecli

install-swapruntime: #? Install binaries with SwapClientService enabled
	@$(call print, "Installing binaries with swapruntime.")
	$(MAKE) install tags="swapruntime"

install-wavewalletrpc: #? Install binaries with wavewalletrpc + swapruntime enabled
	@$(call print, "Installing binaries with wavewalletrpc and swapruntime.")
	$(MAKE) install tags="wavewalletrpc swapruntime"

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
		$(GOBUILD) -trimpath $(RELEASE_LDFLAGS) -tags="$(RELEASE_TAGS)" -o ./bin/merge-sql-schemas-$$sys ./cmd/merge-sql-schemas; \
		echo; \
	done

# ============
# CLEANUP
# ============

clean-networks: #? Remove stale harness Docker networks (use when address pools exhausted)
	@$(call print, "Removing stale ark-harness Docker networks...")
	@docker network ls --filter "name=ark-harness-" -q | xargs -r docker network rm || true
	@echo "Done. Networks removed."

# ============
# HELP
# ============

help: #? Show this help message
	@echo "Available make targets:"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?#\? .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?#\\? "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Examples:"
	@echo "  make build"
	@echo "  make rpc"
	@echo "  make unit tags=\"test_postgres\""
	@echo "  make migrate-create patchname=add_users_table"
