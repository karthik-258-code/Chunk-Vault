BINARY  := chunkvault
PKG     := ./cmd/chunkvault
BIN_DIR := bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test cover lint fmt bench bench-e2e clean

all: lint test build

## build: compile the chunkvault binary into ./bin
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(PKG)

## test: run all tests with the race detector
test:
	go test -race ./...

## cover: run tests with coverage and print per-function totals
cover:
	go test -race -covermode=atomic -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

## lint: run golangci-lint (https://golangci-lint.run/welcome/install/)
lint:
	golangci-lint run ./...

## fmt: format all Go sources
fmt:
	gofmt -s -w cmd internal

## bench: run the chunker benchmarks (reports MB/s)
bench:
	go test -run '^$$' -bench . -benchmem ./internal/chunker/

## bench-e2e: run end-to-end put/get benchmarks
bench-e2e:
	go test -run '^$$' -bench . -benchtime=3s ./cmd/chunkvault/

## clean: remove build and coverage artifacts
clean:
	rm -rf $(BIN_DIR) coverage.out coverage.xml
