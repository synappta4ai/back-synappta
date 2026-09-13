.PHONY: help run build test vet fmt tidy docker-build docker-up docker-down migrate-seed generate-keys

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

run: ## Run the API locally (needs a reachable DATABASE_URL)
	go run .

build: ## Build the binary
	CGO_ENABLED=0 go build -o bin/synapta .

test: ## Run all tests
	go test ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format all Go code
	gofmt -w . && go vet ./...

tidy: ## go mod tidy
	go mod tidy

docker-build: ## Build the Docker image
	docker build -t synapta:local .

docker-up: ## Start the dev stack (Postgres + API)
	docker compose up -d --build

docker-down: ## Stop the dev stack
	docker compose down

generate-keys: ## Generate VAPID + security keys for .env
	go run ./cmd/generate-keys
