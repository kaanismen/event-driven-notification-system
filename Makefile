# Recipes use POSIX sh. On Windows, run make from Git Bash (or WSL).

TEST_POSTGRES_PORT ?= 55432
TEST_DATABASE_URL  ?= postgres://postgres:test@localhost:$(TEST_POSTGRES_PORT)/postgres?sslmode=disable
# Extra flags for go test, e.g. `make test TESTFLAGS=-race` (the race detector needs cgo).
TESTFLAGS ?=

.DEFAULT_GOAL := help

.PHONY: help
help: ## List the available targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "} {printf "  %-14s %s\n", $$1, $$2}'

.PHONY: up
up: ## Build and start the app and Postgres in the background
	docker compose up --build -d

.PHONY: down
down: ## Stop the stack (keeps the database volume)
	docker compose down

.PHONY: logs
logs: ## Follow the app logs
	docker compose logs -f app

.PHONY: build
build: ## Build the binary into ./bin
	go build -o bin/notifier ./cmd/notifier

.PHONY: test
test: test-db-up ## Run unit and integration tests (starts the test database)
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' go test -count=1 $(TESTFLAGS) ./...

.PHONY: test-unit
test-unit: ## Run unit tests only (no Docker needed; integration tests skip)
	go test -count=1 $(TESTFLAGS) ./...

.PHONY: test-db-up
test-db-up: ## Start the disposable test database and wait until it is healthy
	docker compose --profile test up -d --wait postgres-test

.PHONY: test-db-down
test-db-down: ## Stop and remove the test database
	docker compose --profile test rm -sf postgres-test

.PHONY: smoke
smoke: ## Call every API endpoint against the running stack (run make up first)
	bash scripts/smoke.sh

.PHONY: lint
lint: ## Run golangci-lint (includes go vet and gofmt/goimports checks)
	golangci-lint run ./...

.PHONY: fmt
fmt: ## Format all Go code
	golangci-lint fmt ./...
