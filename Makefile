.PHONY: build rebuild up down restart logs shell clean help update-forwarders build-local run-local run-local-udp run-local-udp-debug run-local-udp-prometheus run-local-custom clean-local lint lint-fix fmt fmt-check check

# Docker image name
IMAGE_NAME := retracker
CONTAINER_NAME := retracker

# Default port mapping
PORT := 6969:6969

# Build directory and binary path
BUILD_DIR := bin
BINARY := $(BUILD_DIR)/retracker

help: ## Show this help message
	@echo 'Usage: make [target]'
	@echo ''
	@echo 'Available targets:'
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-15s %s\n", $$1, $$2}'

build: ## Build the Docker image
	docker build -t $(IMAGE_NAME) -f docker/Dockerfile .

rebuild: ## Rebuild the Docker image with latest code from GitHub (no cache)
	docker build --no-cache --pull -t $(IMAGE_NAME) -f docker/Dockerfile .

update-forwarders: ## Update forwarders.yml from online lists and local curated list
	./update-forwarders.sh

up: ## Start the container
	docker compose -f docker/docker-compose.yml up -d

down: ## Stop the container
	docker compose -f docker/docker-compose.yml down

restart: ## Restart the container
	docker compose -f docker/docker-compose.yml restart

logs: ## Show container logs
	docker compose -f docker/docker-compose.yml logs -f

shell: ## Open a shell in the running container
	docker exec -it $(CONTAINER_NAME) /bin/sh

clean: ## Remove container and image
	docker compose -f docker/docker-compose.yml down -v
	docker rmi $(IMAGE_NAME) || true

run: build ## Build and run the container (standalone, not using docker compose)
	docker run -d \
		--name $(CONTAINER_NAME) \
		-p $(PORT) \
		--restart unless-stopped \
		$(IMAGE_NAME)

stop: ## Stop the standalone container
	docker stop $(CONTAINER_NAME) || true
	docker rm $(CONTAINER_NAME) || true

# Advanced usage examples
run-debug: build ## Run with debug mode enabled
	docker run -d \
		--name $(CONTAINER_NAME)-debug \
		-p 6969:80 \
		$(IMAGE_NAME) \
		./retracker -l :6969 -d

run-prometheus: build ## Run with Prometheus metrics enabled
	docker run -d \
		--name $(CONTAINER_NAME)-prom \
		-p 6969:80 \
		$(IMAGE_NAME) \
		./retracker -l :6969 -p

run-custom: build ## Run with custom port (usage: make run-custom PORT=9090:80)
	docker run -d \
		--name $(CONTAINER_NAME)-custom \
		-p $(PORT) \
		$(IMAGE_NAME) \
		./retracker -l :6969

# Local development (non-Docker) commands
build-local: ## Build the Go binary locally
	mkdir -p $(BUILD_DIR) && go build -o $(BINARY) ./cmd/retracker

run-local: build-local ## Run retracker locally with HTTP only (default port 6969)
	$(BINARY) -l :6969 -f ./scripts/lists/forwarders.yml

run-local-udp: build-local ## Run retracker locally with both HTTP and UDP (ports 6969)
	$(BINARY) -l :6969 -u :6969 -f ./scripts/lists/forwarders.yml

run-local-udp-debug: build-local ## Run retracker locally with HTTP, UDP, and debug mode
	$(BINARY) -l :6969 -u :6969 -d -f ./scripts/lists/forwarders.yml

run-local-udp-prometheus: build-local ## Run retracker locally with HTTP, UDP, and Prometheus
	$(BINARY) -l :6969 -u :6969 -p -f ./scripts/lists/forwarders.yml

run-local-custom: build-local ## Run with custom ports (usage: make run-local-custom HTTP_PORT=9090 UDP_PORT=9091)
	$(BINARY) -l :$(HTTP_PORT) -u :$(UDP_PORT) -f ./scripts/lists/forwarders.yml

clean-local: ## Remove the local build directory
	rm -rf $(BUILD_DIR)

# Linting and formatting
# Find golangci-lint in PATH or common Go bin locations
GOLANGCI_LINT := $(shell command -v golangci-lint 2>/dev/null || \
	command -v $$(go env GOPATH)/bin/golangci-lint 2>/dev/null || \
	command -v $$HOME/go/bin/golangci-lint 2>/dev/null || \
	echo "")

lint: ## Run golangci-lint
	@if [ -z "$(GOLANGCI_LINT)" ]; then \
		echo "golangci-lint not found in PATH."; \
		echo "Install with: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; \
		echo "Make sure $$(go env GOPATH)/bin is in your PATH"; \
		exit 1; \
	else \
		echo "Downloading dependencies..."; \
		go mod download && \
		$(GOLANGCI_LINT) run; \
	fi

lint-fix: ## Run golangci-lint with auto-fix
	@if [ -z "$(GOLANGCI_LINT)" ]; then \
		echo "golangci-lint not found in PATH."; \
		echo "Install with: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; \
		echo "Make sure $$(go env GOPATH)/bin is in your PATH"; \
		exit 1; \
	else \
		$(GOLANGCI_LINT) run --fix; \
	fi

# Find gofumpt or gofmt
GOFUMPT := $(shell command -v gofumpt 2>/dev/null || \
	command -v $$(go env GOPATH)/bin/gofumpt 2>/dev/null || \
	command -v $$HOME/go/bin/gofumpt 2>/dev/null || \
	echo "")
GOFMT := $(shell command -v gofmt 2>/dev/null || echo "")

fmt: ## Format code with gofumpt (or gofmt if gofumpt not available)
	@if [ -n "$(GOFUMPT)" ]; then \
		$(GOFUMPT) -l -w .; \
	elif [ -n "$(GOFMT)" ]; then \
		$(GOFMT) -s -w .; \
	else \
		echo "Neither gofumpt nor gofmt found"; \
		echo "Install gofumpt with: go install mvdan.cc/gofumpt@latest"; \
		exit 1; \
	fi

fmt-check: ## Check if code is formatted correctly
	@if [ -n "$(GOFUMPT)" ]; then \
		! $(GOFUMPT) -l . | grep -q . || (echo "Code is not formatted. Run 'make fmt' to fix." && exit 1); \
	elif [ -n "$(GOFMT)" ]; then \
		! $(GOFMT) -l . | grep -q . || (echo "Code is not formatted. Run 'make fmt' to fix." && exit 1); \
	else \
		echo "Neither gofumpt nor gofmt found"; \
		echo "Install gofumpt with: go install mvdan.cc/gofumpt@latest"; \
		exit 1; \
	fi

check: fmt-check lint ## Run all checks (formatting and linting)

