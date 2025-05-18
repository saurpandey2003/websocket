# Makefile for the websocket service

# Variables
SERVICE_NAME := websocket-service
GO := go
PROTOC := protoc
DOCKER := docker
DOCKER_IMAGE := cryptovate/$(SERVICE_NAME)
DOCKER_TAG := latest

# Go build flags
LDFLAGS := -ldflags "-s -w"

# Directories
PROTO_DIR := protos
GEN_DIR := gen

# Main targets
.PHONY: all
all: clean deps proto build

.PHONY: run
run:
	$(GO) run main.go

.PHONY: build
build:
	$(GO) build $(LDFLAGS) -o $(SERVICE_NAME) main.go

.PHONY: clean
clean:
	rm -f $(SERVICE_NAME)
	rm -rf $(GEN_DIR)

.PHONY: deps
deps:
	$(GO) mod download
	$(GO) mod tidy

# Protocol Buffers
.PHONY: proto
proto:
	mkdir -p $(GEN_DIR)
	protoc --go_out=./gen ./protos/websocket/v1/api.proto  --go-grpc_out=./gen

# Docker
.PHONY: docker-build
docker-build:
	$(DOCKER) build -t $(DOCKER_IMAGE):$(DOCKER_TAG) .

.PHONY: docker-run
docker-run:
	$(DOCKER) run -p 8080:8080 -p 9090:9090 $(DOCKER_IMAGE):$(DOCKER_TAG)

# Testing
.PHONY: test
test:
	$(GO) test -v ./...

.PHONY: test-coverage
test-coverage:
	$(GO) test -v -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

# Development
.PHONY: dev
dev:
	air -c .air.toml

# Help
.PHONY: help
help:
	@echo "Available targets:"
	@echo "  all            - Clean, download dependencies, generate proto files, and build"
	@echo "  run            - Run the service"
	@echo "  build          - Build the service"
	@echo "  clean          - Remove build artifacts"
	@echo "  deps           - Download dependencies"
	@echo "  proto          - Generate code from Protocol Buffers"
	@echo "  docker-build   - Build Docker image"
	@echo "  docker-run     - Run Docker container"
	@echo "  test           - Run tests"
	@echo "  test-coverage  - Run tests with coverage"
	@echo "  dev            - Run with hot reload (requires air)"
	@echo "  help           - Show this help"
