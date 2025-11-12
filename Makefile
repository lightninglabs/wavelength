.PHONY: sqlc sqlc-check migrate-create migrate-up migrate-down gen

# =========
# VARIABLES
# =========

GO_BIN := $(GOPATH)/bin
MIGRATE_BIN := $(GO_BIN)/migrate

# DB connection string for migrations (example).
DB_CONNECTIONSTRING ?= sqlite://./darepo.db

# Print helper.
define print
	@echo "===> $(1)"
endef

# ============
# SQLC TARGETS
# ============

sqlc:
	@$(call print, "Generating sql models and queries in Go")
	./scripts/gen_sqlc_docker.sh
	@$(call print, "Merging SQL migrations into consolidated schemas")
	go run ./cmd/merge-sql-schemas/main.go

sqlc-check: sqlc
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

# Install golang-migrate if not present.
$(MIGRATE_BIN):
	@$(call print, "Installing golang-migrate")
	go install -tags 'postgres sqlite3' github.com/golang-migrate/migrate/v4/cmd/migrate@latest

migrate-create: $(MIGRATE_BIN)
	@$(call print, "Creating migration: $(patchname)")
	@if [ -z "$(patchname)" ]; then \
		echo "Error: patchname is required. Usage: make migrate-create patchname=add_new_table"; \
		exit 1; \
	fi
	migrate create -dir db/sqlc/migrations -seq -ext sql $(patchname)

migrate-up: $(MIGRATE_BIN)
	@$(call print, "Applying all migrations")
	migrate -path db/sqlc/migrations -database $(DB_CONNECTIONSTRING) -verbose up

migrate-down: $(MIGRATE_BIN)
	@$(call print, "Rolling back one migration")
	migrate -path db/sqlc/migrations -database $(DB_CONNECTIONSTRING) -verbose down 1

# Main code generation target.
gen: sqlc

# ============
# HELP
# ============

help:
	@echo "Available make targets:"
	@echo ""
	@echo "  sqlc               - Generate SQL code from schema and queries"
	@echo "  sqlc-check         - Verify SQL code generation is up to date"
	@echo "  migrate-create     - Create a new migration (requires patchname=...)"
	@echo "  migrate-up         - Apply all pending migrations"
	@echo "  migrate-down       - Roll back one migration"
	@echo "  gen                - Generate all code (sqlc, etc.)"
	@echo ""
	@echo "Examples:"
	@echo "  make sqlc"
	@echo "  make migrate-create patchname=add_users_table"
	@echo "  make migrate-up DB_CONNECTIONSTRING=postgres://user:pass@localhost/dbname"
