# Local development targets. CI runs the same checks through the official
# actions in .github/workflows; keep the golangci-lint version in sync with ci.yml.
SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c

BIN           := $(CURDIR)/bin
GOLANGCI_LINT := $(BIN)/golangci-lint
APP           := agy-gateway
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS       := -s -w -X main.version=$(VERSION)
IMAGE         ?= agy-gateway:$(VERSION)

.PHONY: all bootstrap fmt fmt-check lint vet test test-race test-integration check build image clean

all: check build

## bootstrap: install pinned dev tools into ./bin
bootstrap:
	./scripts/bootstrap.sh

$(GOLANGCI_LINT):
	./scripts/bootstrap.sh

## fmt: rewrite sources with the configured formatters
fmt: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) fmt ./...

## fmt-check: fail when any file is not formatted
fmt-check: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) fmt --diff ./...

## lint: verify the lint config and run golangci-lint
lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) config verify
	$(GOLANGCI_LINT) run ./...

vet:
	go vet ./...
	go vet -tags integration ./...

test:
	go test -count=1 ./...

test-race:
	go test -race -count=1 ./...

## test-integration: real agy runtime tests (needs a signed-in local agy)
test-integration:
	go test -tags integration -count=1 -timeout 30m -v ./test/integration/...

check: fmt-check lint vet test test-race

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/$(APP) ./cmd/$(APP)

image:
	docker build -t $(IMAGE) .

clean:
	rm -f $(BIN)/$(APP)
