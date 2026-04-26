# Apex Makefile — common developer tasks
# Run `make help` to see all targets.

.DEFAULT_GOAL := help
BINARY_NAME    := apex-server
BUILD_DIR      := ./bin
MAIN_PKG       := ./cmd/server
MIGRATIONS_DIR := ./migrations
DOCKER_COMPOSE := docker compose

# Inject git metadata into binary
VERSION        := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS        := -ldflags "-s -w -X main.version=$(VERSION)"

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

# ─── Build ────────────────────────────────────────────────────────────────────

.PHONY: build
build: ## Build the binary
	@echo "Building $(BINARY_NAME) $(VERSION)..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 go build $(LDFLAGS) -trimpath -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN_PKG)
	@echo "Built: $(BUILD_DIR)/$(BINARY_NAME)"

.PHONY: build-linux
build-linux: ## Build for Linux/amd64 (for Docker)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -trimpath -o $(BUILD_DIR)/$(BINARY_NAME)-linux $(MAIN_PKG)

# ─── Run ──────────────────────────────────────────────────────────────────────

.PHONY: run
run: ## Run the server locally (requires .env)
	go run $(MAIN_PKG)

.PHONY: dev
dev: ## Run with hot reload (requires Air: go install github.com/air-verse/air@latest)
	air -c .air.toml

# ─── Test ─────────────────────────────────────────────────────────────────────

.PHONY: test
test: ## Run all tests
	go test -v -race -count=1 ./...

.PHONY: test-coverage
test-coverage: ## Run tests with coverage report
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

.PHONY: test-integration
test-integration: ## Run integration tests (requires running Docker stack)
	go test -v -race -tags=integration -count=1 ./...

.PHONY: bench
bench: ## Run benchmarks
	go test -bench=. -benchmem -run=^$$ ./...

# ─── Lint & Format ────────────────────────────────────────────────────────────

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run ./...

.PHONY: fmt
fmt: ## Format all Go code
	gofmt -w -s .
	goimports -w .

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: check
check: fmt vet lint test ## Run all checks (fmt, vet, lint, test)

# ─── Database ─────────────────────────────────────────────────────────────────

.PHONY: migrate-up
migrate-up: ## Apply all pending migrations
	migrate -path $(MIGRATIONS_DIR) -database "$(DATABASE_URL)" up

.PHONY: migrate-down
migrate-down: ## Roll back one migration
	migrate -path $(MIGRATIONS_DIR) -database "$(DATABASE_URL)" down 1

.PHONY: migrate-status
migrate-status: ## Show migration status
	migrate -path $(MIGRATIONS_DIR) -database "$(DATABASE_URL)" version

.PHONY: migrate-create
migrate-create: ## Create a new migration (usage: make migrate-create NAME=add_foo_table)
	@test -n "$(NAME)" || (echo "Usage: make migrate-create NAME=your_migration_name" && exit 1)
	migrate create -ext sql -dir $(MIGRATIONS_DIR) -seq $(NAME)

# ─── Docker ───────────────────────────────────────────────────────────────────

.PHONY: up
up: ## Start all services
	$(DOCKER_COMPOSE) up -d

.PHONY: down
down: ## Stop all services
	$(DOCKER_COMPOSE) down

.PHONY: logs
logs: ## Tail API logs
	$(DOCKER_COMPOSE) logs -f api

.PHONY: docker-build
docker-build: ## Build production Docker image
	docker build -t apex:$(VERSION) -f deployments/docker/Dockerfile --target production .

.PHONY: ps
ps: ## Show running containers
	$(DOCKER_COMPOSE) ps

# ─── Utilities ────────────────────────────────────────────────────────────────

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	go mod tidy

.PHONY: deps
deps: ## Download dependencies
	go mod download

.PHONY: generate
generate: ## Run go generate
	go generate ./...

.PHONY: seed
seed: ## Seed the database with sample data
	go run ./scripts/seed/main.go

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BUILD_DIR) coverage.out coverage.html
