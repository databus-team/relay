# Makefile for file-exchange

BINARY_NAME := file-exchange
BUILD_DIR := .
MAIN_PATH := ./cmd/file-exchange

# Go parameters
GOCMD := go
GOBUILD := $(GOCMD) build
GOTEST := $(GOCMD) test
GOCLEAN := $(GOCMD) clean
GOGET := $(GOCMD) get
GOMOD := $(GOCMD) mod

.PHONY: all build test clean install fmt lint deps help

all: build

## build: Build the binary
build:
	$(GOBUILD) -o $(BINARY_NAME) $(MAIN_PATH)

## build-debug: Build with debug symbols
build-debug:
	$(GOBUILD) -gcflags="all=-N -l" -o $(BINARY_NAME) $(MAIN_PATH)

## test: Run all tests
test:
	$(GOTEST) -v -race ./...

## test-coverage: Run tests with coverage
test-coverage:
	$(GOTEST) -v -race -coverprofile=coverage.out ./...
	$(GOCMD) tool cover -html=coverage.out -o coverage.html

## clean: Clean build artifacts
clean:
	$(GOCLEAN)
	rm -f $(BINARY_NAME)
	rm -f coverage.out coverage.html

## install: Install binary to GOPATH
install:
	$(GOCMD) install $(MAIN_PATH)

## deps: Download dependencies
deps:
	$(GOMOD) download
	$(GOMOD) tidy

## fmt: Format code
fmt:
	$(GOCMD) fmt ./...

## lint: Run linter (requires golangci-lint)
lint:
	golangci-lint run ./...

## vet: Run go vet
vet:
	$(GOCMD) vet ./...

## run: Run the application
run: build
	./$(BINARY_NAME) watch --config ~/.file-exchange/config.yaml

## run-once: Run single iteration
run-once: build
	./$(BINARY_NAME) watch --config ~/.file-exchange/config.yaml --once

## help: Show this help
help:
	@echo "Makefile for file-exchange"
	@echo ""
	@echo "Usage:"
	@echo "  make build          Build the binary"
	@echo "  make test          Run all tests"
	@echo "  make clean         Clean build artifacts"
	@echo "  make install       Install binary to GOPATH"
	@echo "  make deps          Download dependencies"
	@echo "  make fmt           Format code"
	@echo "  make lint          Run linter"
	@echo "  make run           Run in daemon mode"
	@echo "  make run-once      Run single iteration"
	@echo "  make help          Show this help"