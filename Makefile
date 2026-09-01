# Makefile for relay

BINARY_NAME := relay
MAIN_PATH := ./cmd/relay

GOCMD := go
GOBUILD := $(GOCMD) build
GOTEST := $(GOCMD) test
GOCLEAN := $(GOCMD) clean
GOMOD := $(GOCMD) mod

# 版本元信息:stamp 进 internal/version(所有平台产物共享同一递交信息,便于 relay version 跨机对比)。
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS_STAMP := -X github.com/user/relay/internal/version.Version=$(VERSION) -X github.com/user/relay/internal/version.Commit=$(COMMIT) -X github.com/user/relay/internal/version.Date=$(DATE)

.PHONY: all build build-release build-linux build-windows build-debug test test-coverage clean install deps fmt vet run deploy deploy-remote deploy-transit help

all: build

## build: Build the binary (development)
build:
	$(GOBUILD) -o $(BINARY_NAME) $(MAIN_PATH)

## build-release: Build optimized binary for production (stamped with version)
build-release:
	CGO_ENABLED=0 $(GOBUILD) -ldflags="-s -w $(LDFLAGS_STAMP)" -o $(BINARY_NAME) $(MAIN_PATH)

## build-linux: Cross-compile for Linux x64 (stamped)
build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GOBUILD) -ldflags="-s -w $(LDFLAGS_STAMP)" -o $(BINARY_NAME)-linux $(MAIN_PATH)

## build-windows: Cross-compile for Windows x64 (stamped)
build-windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GOBUILD) -ldflags="-s -w $(LDFLAGS_STAMP)" -o $(BINARY_NAME).exe $(MAIN_PATH)

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

## install: Build optimized binary and install to ~/.local/bin (on PATH)
install: build-release
	install -m 0755 $(BINARY_NAME) $(HOME)/.local/bin/$(BINARY_NAME)

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

## deploy-remote: Auto-deliver new binary to remote executor via relay (RESTART=1 to swap+restart)
deploy-remote:
	./scripts/relay-deploy.sh remote

## deploy-transit: Build linux binary + print manual code-server steps for the transit server
deploy-transit:
	./scripts/relay-deploy.sh transit

## deploy: Both remote and transit
deploy:
	./scripts/relay-deploy.sh all

## help: Show this help
help:
	@grep -E '^[##]+' $(MAKEFILE_LIST) | head -30
	@echo ""
	@echo "Usage:"
	@echo "  make build          Build the binary"
	@echo "  make build-linux    Cross-compile for Linux x64"
	@echo "  make build-windows  Cross-compile for Windows x64"
	@echo "  make test           Run all tests"
	@echo "  make clean          Clean build artifacts"
	@echo "  make install        Build and install to ~/.local/bin"
	@echo "  make deps           Download dependencies"
	@echo "  make fmt            Format code"
	@echo "  make vet            Run go vet"
	@echo "  make run            Run in daemon mode"
	@echo "  make deploy-remote    Auto-deliver to remote executor (RESTART=1 to swap+restart)"
	@echo "  make deploy-transit   Build + print manual transit server steps"
	@echo "  make deploy           Both above"
	@echo "  make help           Show this help"