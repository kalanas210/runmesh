# RunMesh — developer task runner.
# Windows users without GNU make: use ./task.ps1 <target> instead (same targets).

BINARY := runmesh
BIN_DIR := bin
PKG := ./...

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

.PHONY: test
test: ## Run all tests
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

.PHONY: tidy
tidy: ## Tidy go.mod
	go mod tidy

.PHONY: clean
clean: ## Remove build and coverage output
	rm -rf $(BIN_DIR) coverage.out coverage.html
