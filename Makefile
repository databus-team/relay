# Makefile for relay

BINARY_NAME := relay
MAIN_PATH := ./cmd/relay

GOCMD := go
GOBUILD := $(GOCMD) build
GOTEST := $(GOCMD) test
GOCLEAN := $(GOCMD) clean
GOMOD := $(GOCMD) mod

.PHONY: all build test clean install fmt vet help

all: build

## build: Build the binary (development)
build:
	$(GOBUILD) -o $(BINARY_NAME) $(MAIN_PATH)

## build-release: Build optimized binary for production
build-release:
	CGO_ENABLED=0 $(GOBUILD) -ldflags="-s -w" -o $(BINARY_NAME) $(MAIN_PATH)

## build-windows: Cross-compile for Windows x64
build-windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GOBUILD) -ldflags="-s -w" -o $(BINARY_NAME).exe $(MAIN_PATH)

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

## vet: Run go vet
vet:
	$(GOCMD) vet ./...

## run: Run in daemon mode (continuous watch)
run: build
	./$(BINARY_NAME) watch

## pull: Download single file from remote
pull:
	./$(BINARY_NAME) pull -c config.yaml -w <watch_id> <filename>

## list: List remote directory contents
list:
	./$(BINARY_NAME) list -c config.yaml -w <watch_id>

## cleanup: Remove stale command files from remote
cleanup:
	./$(BINARY_NAME) cleanup -c config.yaml -w <watch_id>

## push: Push file to remote
push:
	./$(BINARY_NAME) push --watch=<watch_id> <source>

## exec: Forward command to remote
exec:
	./$(BINARY_NAME) exec -w <watch_id> <command>

## help: Show this help
help:
	@grep -E '^[##]+' $< | head -20
	@echo ""
	@echo "Usage:"
	@echo "  make build          Build the binary"
	@echo "  make test           Run all tests"
	@echo "  make clean          Clean build artifacts"
	@echo "  make install        Install binary to GOPATH"
	@echo "  make deps           Download dependencies"
	@echo "  make fmt            Format code"
	@echo "  make vet            Run go vet"
	@echo "  make run            Run in daemon mode"
	@echo "  make help           Show this help"