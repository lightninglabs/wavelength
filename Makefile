.PHONY: sqlc sqlc-check migrate-create migrate-up migrate-down gen
.PHONY: lint lint-source docker-tools fmt fmt-check tidy-module tidy-module-check
.PHONY: unit unit-cover unit-race check-go-version release
.PHONY: build rpc install help

# Default target
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
LOG_TAGS := nolog
TEST_FLAGS :=
RELEASE_TAGS := kvdb_postgres kvdb_sqlite

ifneq ($(tags),)
DEV_TAGS += ${tags}
endif

# Coverage settings.
COVER_PKG = $$($(GOCC) list -deps -tags="$(DEV_TAGS)" ./... | grep '$(PKG)')
COVER_FLAGS = -coverprofile=coverage.txt -covermode=atomic -coverpkg=$(PKG)/...

# Test commands.
GOLIST := $(GOCC) list -tags="$(DEV_TAGS)" -deps $(PKG)/... | grep '$(PKG)'| grep -v '/vendor/'
UNIT := $(GOLIST) | $(XARGS) env $(GOTEST) -tags="$(DEV_TAGS) $(LOG_TAGS)" $(TEST_FLAGS)
UNIT_RACE := $(UNIT) -race
UNIT_COVER := $(GOTEST) $(COVER_FLAGS) -tags="$(DEV_TAGS) $(LOG_TAGS)" $(TEST_FLAGS) $(COVER_PKG)

# Linting uses a lot of memory, so keep it under control by limiting the number
# of workers if requested.
ifneq ($(workers),)
LINT_WORKERS = --concurrency=$(workers)
endif

# Apply the optimized cache mounting from PR #10202.
DOCKER_TOOLS = docker run \
  --rm \
  -v $(shell bash -c "mkdir -p /tmp/go-build-cache; echo /tmp/go-build-cache"):/root/.cache/go-build \
  -v $(shell bash -c "mkdir -p /tmp/go-lint-cache; echo /tmp/go-lint-cache"):/root/.cache/golangci-lint \
  -v $$(pwd):/build darepo-tools

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

lint: check-go-version lint-source #? Run static code analysis

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

unit-cover: #? Run unit tests with coverage
	@$(call print, "Running unit coverage tests.")
	$(UNIT_COVER)

unit-race: #? Run unit tests with race detector
	@$(call print, "Running unit race tests.")
	env CGO_ENABLED=1 GORACE="history_size=7 halt_on_errors=1" $(UNIT_RACE)

# ============
# RPC GENERATION
# ============

rpc: #? Generate RPC stubs from proto files (uses Docker)
	@$(call print, "Generating RPC stubs from proto files using Docker.")
	./scripts/gen_protos_docker.sh

# ============
# BUILDING
# ============

build: #? Build arkd and arkcli binaries
	@$(call print, "Building arkd and arkcli.")
	$(GOBUILD) -o ./arkd ./cmd/arkd
	$(GOBUILD) -o ./arkcli ./cmd/arkcli

install: #? Install arkd and arkcli to GOPATH/bin
	@$(call print, "Installing arkd and arkcli.")
	$(GOINSTALL) -tags="$(DEV_TAGS)" ./cmd/arkd
	$(GOINSTALL) -tags="$(DEV_TAGS)" ./cmd/arkcli

# ============
# INSTALLATION & RELEASE
# ============

release: #? Cross compile for all supported platforms
	@$(call print, "Cross compiling release binaries.")
	@for sys in $(BUILD_SYSTEM); do \
		echo "Building for $$sys"; \
		export CGO_ENABLED=0 GOOS=$$(echo $$sys | cut -d- -f1) GOARCH=$$(echo $$sys | cut -d- -f2); \
		if [ "$$GOARCH" = "armv6" ]; then \
			export GOARCH=arm; export GOARM=6; \
		elif [ "$$GOARCH" = "armv7" ]; then \
			export GOARCH=arm; export GOARM=7; \
		fi; \
		$(GOBUILD) -trimpath -tags="$(RELEASE_TAGS)" -o /tmp/darepo-$$sys ./cmd/...; \
		echo; \
	done

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
	@echo "  make unit tags=\"test_db_postgres\""
	@echo "  make migrate-create patchname=add_users_table"
