# uniubot — Go build, test and maintenance entry points.
#
#   make build     build the host and every Go plugin into bin/ and plugins/*/
#   make plugins   build only the Go plugins
#   make run       run the host from the working tree
#   make test      run the Go test suite (unit + RPC integration + API tests)
#   make race      run the concurrency-sensitive packages with -race
#   make lint      gofmt check + go vet
#   make frontend  build the dashboard (requires Node.js)
#   make clean     remove build artifacts

SHELL := /bin/bash
GO ?= go
BIN_DIR := bin
BINARY := $(BIN_DIR)/uniubot
VERSION ?= 1.0.0
LDFLAGS := -X github.com/uniu-network/uniubot/internal/bootstrap.Version=$(VERSION)

GO_PLUGIN_DIRS := $(patsubst %/plugin.json,%,$(wildcard plugins/*/plugin.json) $(wildcard plugins/examples/*/plugin.json))
GO_PLUGINS := $(shell grep -l '"runtime": "go"' $(wildcard plugins/*/plugin.json plugins/examples/*/plugin.json) 2>/dev/null | xargs -r -n1 dirname)

.PHONY: all build plugins run test race lint fmt vet frontend clean install-python check

all: build

build: plugins
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/uniubot
	@echo "built $(BINARY)"

# Builds every Go plugin next to its manifest (plugin.json expects "./plugin").
plugins:
	@for dir in $(GO_PLUGINS); do \
		echo "building plugin $$dir"; \
		$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$$dir/plugin" "./$$dir" || exit 1; \
	done

run:
	$(GO) run ./cmd/uniubot

test:
	$(GO) test ./...

race:
	$(GO) test -race ./internal/bot/ ./internal/plugins/ ./internal/api/ ./internal/storage/ ./internal/services/ ./internal/adapters/... ./pkg/pluginrpc/

fmt:
	gofmt -w cmd internal pkg plugins

vet:
	$(GO) vet ./...

lint: vet
	@files=$$(gofmt -l cmd internal pkg plugins); \
	if [ -n "$$files" ]; then echo "gofmt required for:"; echo "$$files"; exit 1; fi
	@echo "gofmt clean"
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not installed, skipped"; fi

frontend:
	cd frontend && npm install && npm run build

# Creates the Conda environment used by the Python example plugin.
install-python:
	conda env create -f plugins/examples/python/environment.yml

check: lint test
	@echo "all checks passed"

clean:
	rm -rf $(BIN_DIR)
	rm -f plugins/*/plugin plugins/examples/*/plugin
	rm -rf frontend/dist
