# RunMesh — developer task runner.
# Windows users without GNU make: use ./task.ps1 <target> instead (same targets).

BINARY := runmesh
BIN_DIR := bin
PKG := ./...

# The host port the local PostgreSQL is published on. A native PostgreSQL owns
# 5432 on a lot of developer machines, so this is overridable and the override
# reaches both docker compose and the connection string:
#
#   make test-pg RUNMESH_DB_PORT=5433
RUNMESH_DB_PORT ?= 5432
export RUNMESH_DB_PORT

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Compile the server into ./bin
	go build -o $(BIN_DIR)/$(BINARY) ./cmd/server

.PHONY: run
run: ## Run the server from source
	go run ./cmd/server

.PHONY: db-up
db-up: ## Start the local PostgreSQL and wait for it
	docker compose up -d --wait postgres

.PHONY: db-down
db-down: ## Stop the local PostgreSQL, keeping its data
	docker compose stop postgres

.PHONY: db-reset
db-reset: ## Destroy the local PostgreSQL and its data
	docker compose down -v

.PHONY: test
test: ## Run all tests (PostgreSQL cases skip without RUNMESH_TEST_DATABASE_URL)
	go test -count=1 $(PKG)

.PHONY: race
race: ## Run all tests under the race detector
	go test -race -count=1 $(PKG)

.PHONY: cover
cover: ## Run tests and open an HTML coverage report
	go test -race -count=1 -covermode=atomic -coverprofile=coverage.out $(PKG)
	go tool cover -func=coverage.out | tail -n 1
	go tool cover -html=coverage.out -o coverage.html

.PHONY: bench
bench: ## Run benchmarks
	go test -run '^$$' -bench . -benchmem $(PKG)

.PHONY: vet
vet: ## Run go vet
	go vet $(PKG)

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -s -w .

.PHONY: lint
lint: ## Check formatting and run vet
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "not gofmt'd:"; echo "$$unformatted"; exit 1; fi
	go vet $(PKG)

.PHONY: check
check: lint race ## Everything CI runs

# The PostgreSQL conformance suite and the crash-recovery test SKIP when this is
# unset, so `make test` stays useful without a database. This target is the one
# that proves the durable store actually works.
.PHONY: test-pg
test-pg: db-up ## Run the suite against the local PostgreSQL
	RUNMESH_TEST_DATABASE_URL='postgres://runmesh:runmesh@127.0.0.1:$(RUNMESH_DB_PORT)/runmesh?sslmode=disable' go test -race -count=1 $(PKG)

.PHONY: tidy
tidy: ## Tidy go.mod
	go mod tidy

.PHONY: clean
clean: ## Remove build and coverage output
	rm -rf $(BIN_DIR) coverage.out coverage.html
