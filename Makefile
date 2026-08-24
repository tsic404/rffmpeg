# rffmpeg Makefile
# Build script for Go 1.23 project

# Variables
BINARY_SERVER := rffmpeg-server
BINARY_WORKER := rffmpeg-worker
BINARY_CLI := rffmpeg
BIN_DIR := bin
GO := go
GOFLAGS := -v

# Source paths
CMD_SERVER := ./cmd/server
CMD_WORKER := ./cmd/worker
CMD_CLI := ./cmd/cli

# Build flags
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S')
LDFLAGS := -ldflags "-s -w -X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME)"

# Default target
.DEFAULT_GOAL := help

# Build targets
.PHONY: build build-server build-worker build-cli

build: build-server build-worker build-cli ## Build all components

build-server: ## Build server component
	@echo "Building $(BINARY_SERVER)..."
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/$(BINARY_SERVER) $(CMD_SERVER)

build-worker: ## Build worker component
	@echo "Building $(BINARY_WORKER)..."
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/$(BINARY_WORKER) $(CMD_WORKER)

build-cli: ## Build CLI component
	@echo "Building $(BINARY_CLI)..."
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/$(BINARY_CLI) $(CMD_CLI)

# Test media generation
.PHONY: generate-test-media setup-testdata

generate-test-media: ## Generate test media files for QA testing
	@echo "Generating test media files..."
	@mkdir -p test_data
	@ffmpeg -y -f lavfi -i testsrc=duration=5:size=1280x720:rate=30 \
		-f lavfi -i sine=frequency=440:duration=5 \
		-c:v libx264 -preset ultrafast -crf 28 \
		-c:a aac -b:a 128k \
		-shortest \
		test_data/test-720p.mp4
	@echo "test-720p.mp4 generated successfully."

setup-testdata: ## Setup test data directories for QA testing
	@./scripts/setup-testdata.sh

# QA Test Infrastructure targets (S8.1/S8.2)
.PHONY: start-http-fileserver start-rtmp-server

start-http-fileserver: ## Start HTTP file server for S8.1 remote input testing (default port: 18080)
	@./scripts/start-http-fileserver.sh

start-rtmp-server: ## Start RTMP stream receiver for S8.2 streaming output testing (default port: 1935)
	@./scripts/start-rtmp-server.sh

# Test targets
.PHONY: test test-race test-coverage

test: ## Run all tests
	@echo "Running tests..."
	$(GO) test -v ./...

test-race: ## Run all tests with the race detector (includes concurrency regression tests)
	@echo "Running race tests..."
	$(GO) test -race ./...

test-coverage: ## Run tests with coverage report
	@echo "Running tests with coverage..."
	$(GO) test -v -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

# Code quality targets
.PHONY: lint fmt

lint: ## Run code linter (go vet)
	@echo "Running linter..."
	$(GO) vet ./...

fmt: ## Format code
	@echo "Formatting code..."
	$(GO) fmt ./...

# Clean target
.PHONY: clean

clean: ## Clean build artifacts
	@echo "Cleaning build artifacts..."
	rm -rf $(BIN_DIR)
	rm -f coverage.out coverage.html

# Install target
.PHONY: install

install: build ## Install binaries to system path
	@echo "Installing binaries..."
	install -m 755 $(BIN_DIR)/$(BINARY_SERVER) /usr/local/bin/
	install -m 755 $(BIN_DIR)/$(BINARY_WORKER) /usr/local/bin/
	install -m 755 $(BIN_DIR)/$(BINARY_CLI) /usr/local/bin/

# Development targets
.PHONY: dev run

dev: ## Start development environment
	@echo "Starting development environment..."
	@echo "Run 'make run' to start server and worker, or use the following commands:"
	@echo "  ./bin/rffmpeg-server   - Start the server"
	@echo "  ./bin/rffmpeg-worker   - Start a worker"
	@echo "  ./bin/rffmpeg --help   - CLI help"

run: build ## Run server and worker locally
	@echo "Starting server and worker..."
	@echo "Note: Run the following in separate terminals:"
	@echo "  Terminal 1: ./bin/rffmpeg-server"
	@echo "  Terminal 2: ./bin/rffmpeg-worker"

# Help target
.PHONY: help

help: ## Show this help message
	@echo "rffmpeg Build System"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-20s %s\n", $$1, $$2}'
