# RunMesh — developer task runner.
# Windows users without GNU make: use ./task.ps1 <target> instead (same targets).

BINARY := runmesh
BIN_DIR := bin

# The Go packages this repository owns, named rather than globbed.
#
# NOT `./...`, and the reason arrived with Week 6: web/ carries a node_modules
# tree, and npm packages sometimes ship Go source of their own — `flatted/golang`
# is one that does. `./...` descends into it and puts a stranger's package into
# this module's build graph. That is harmless right up to the day it does not
# compile, at which point `make test` fails for a reason that has nothing to do
# with RunMesh and whoever is holding it goes looking in the wrong repository.
# Naming the four roots costs one line and removes the whole class.
PKG := ./cmd/... ./internal/... ./migrations/... ./tests/...

# The same argument for gofmt, which walks a directory tree rather than a
# package pattern and so cannot be told about node_modules any other way.
GOSRC := cmd internal migrations tests

# The host port the local PostgreSQL is published on. A native PostgreSQL owns
# 5432 on a lot of developer machines, so this is overridable and the override
# reaches both docker compose and the connection string:
#
#   make test-pg RUNMESH_DB_PORT=5433
RUNMESH_DB_PORT ?= 5432
export RUNMESH_DB_PORT

# The host ports the monitoring stack is published on, overridable exactly like
# the database port and for exactly the same reason. Grafana's own default is
# 3000 and is deliberately not used: `next dev` in web/ takes 3000.
RUNMESH_PROMETHEUS_PORT ?= 9090
export RUNMESH_PROMETHEUS_PORT
RUNMESH_GRAFANA_PORT ?= 3001
export RUNMESH_GRAFANA_PORT

# The host port the local Redis is published on, overridable exactly like the
# database port and for exactly the same reason:
#
#   make test-redis RUNMESH_REDIS_PORT=6380
RUNMESH_REDIS_PORT ?= 6379
export RUNMESH_REDIS_PORT

# The k6 script `make load` runs. Overridable the same way, so a second
# scenario is `make load K6_SCRIPT=tests/load/soak.js` rather than a second
# target that has to be kept in step with this one.
K6_SCRIPT ?= tests/load/smoke.js

.DEFAULT_GOAL := help

# The column is 16 wide rather than 14 because `test-integration` is 16
# characters. Note also that the character class excludes digits, so a target
# named `k6` would run perfectly and never appear in this list — which is why
# the k6 target is called `load`.
.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

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

# Prometheus and Grafana sit behind the `monitoring` compose profile, which is
# what keeps `db-up` — and every other compose command in this file — pulling
# and waiting for nothing new. RunMesh itself is not a compose service, so
# start the server separately and Prometheus will find it on
# host.docker.internal:8080.
.PHONY: monitoring-up
monitoring-up: ## Start Prometheus and Grafana and wait for them
	docker compose --profile monitoring up -d --wait prometheus grafana
	@echo "prometheus  http://127.0.0.1:$(RUNMESH_PROMETHEUS_PORT)/targets"
	@echo "grafana     http://127.0.0.1:$(RUNMESH_GRAFANA_PORT)/d/runmesh-overview"

# Stop, not down: the containers go away on the next `db-reset` along with
# everything else, and stopping keeps the samples already collected so that
# "it was slow ten minutes ago" is still a question Prometheus can answer.
.PHONY: monitoring-down
monitoring-down: ## Stop Prometheus and Grafana, keeping their data
	docker compose --profile monitoring stop prometheus grafana

# Redis sits behind its own `ratelimit` compose profile, exactly like
# monitoring: `db-up` and every other compose command here keep pulling and
# waiting for nothing new. Rate limiting is itself opt-in — see ADR 0015 —
# so its dependency costs nothing until asked for either.
.PHONY: redis-up
redis-up: ## Start the local Redis and wait for it
	docker compose --profile ratelimit up -d --wait redis

.PHONY: redis-down
redis-down: ## Stop the local Redis (no data to keep; see docker-compose.yml)
	docker compose --profile ratelimit stop redis

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

# k6 drives a RunMesh that is already running; it does not start one, because a
# load generator that owns the lifecycle of the thing it measures cannot be
# pointed at a different one. Start the server first, then:
#
#   RUNMESH_URL=http://127.0.0.1:8080 RUNMESH_API_KEY=<key> make load
#
# k6 passes the whole process environment through to __ENV by default, so the
# script reads those without any -e plumbing here.
.PHONY: load
load: ## Run the k6 load script against a running server
	k6 run $(K6_SCRIPT)

# The dashboard has its own toolchain, and these targets exist so `make help`
# describes the whole project rather than the Go half of it. They are thin on
# purpose: anybody working on the dashboard for more than a minute will run npm
# from web/ directly, and a wrapper trying to be more than a shortcut would just
# be a second place for package.json's scripts to drift from.
#
# `npm ci` rather than `npm install`: it installs exactly what the lockfile pins
# and fails if the lockfile and package.json disagree, which is what makes the
# install reproducible rather than merely repeated.
.PHONY: web-install
web-install: ## Install the dashboard's dependencies from the lockfile
	cd web && npm ci

.PHONY: web-dev
web-dev: ## Run the dashboard in development mode on :3000
	cd web && npm run dev

.PHONY: web-build
web-build: ## Build the dashboard (which is also its type check)
	cd web && npm run build

.PHONY: web-lint
web-lint: ## Lint the dashboard
	cd web && npm run lint

.PHONY: web-test
web-test: ## Run the dashboard's unit tests
	cd web && npm test

.PHONY: vet
vet: ## Run go vet
	go vet $(PKG)

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -s -w $(GOSRC)

.PHONY: lint
lint: ## Check formatting and run vet
	@unformatted=$$(gofmt -l $(GOSRC)); \
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

# The integration suite drives the assembled server rather than a package, so
# it is slow, it needs a database, and it is behind the `integration` build tag
# — which is what keeps plain `make test` a unit-test loop somebody will
# actually run between edits.
.PHONY: test-integration
test-integration: db-up ## Run the integration suite against the local PostgreSQL
	RUNMESH_TEST_DATABASE_URL='postgres://runmesh:runmesh@127.0.0.1:$(RUNMESH_DB_PORT)/runmesh?sslmode=disable' go test -count=1 -tags=integration ./tests/integration/...

# internal/redis and internal/ratelimit SKIP when this is unset, exactly the
# shape RUNMESH_TEST_DATABASE_URL gives the PostgreSQL suite. This is the
# target that proves the hand-written RESP client and the token bucket it
# backs actually work — see ADR 0015.
.PHONY: test-redis
test-redis: redis-up ## Run the suite against the local Redis
	RUNMESH_TEST_REDIS_URL='redis://127.0.0.1:$(RUNMESH_REDIS_PORT)' go test -race -count=1 $(PKG)

.PHONY: tidy
tidy: ## Tidy go.mod
	go mod tidy

.PHONY: clean
clean: ## Remove build and coverage output
	rm -rf $(BIN_DIR) coverage.out coverage.html web/.next web/out
