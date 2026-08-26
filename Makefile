BINARY  := pgsink
BIN_DIR := bin
PKG     := main

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.1.0-dev")
LDFLAGS := -ldflags "-X $(PKG).version=$(VERSION)"

.DEFAULT_GOAL := help

.PHONY: build
build: ## Build the pgsink binary into bin/
	@mkdir -p $(BIN_DIR)
	go build $(LDFLAGS) -o $(BIN_DIR)/$(BINARY) ./cmd/pgsink
	@echo "→ $(BIN_DIR)/$(BINARY)  ($(VERSION))"

.PHONY: test
test: ## Run all tests
	go test ./...

.PHONY: check
check: vet test build ## Vet + test + build (use in CI)

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -w .

.PHONY: doctor
doctor: build ## Check the catalog against the local Apiary database
	$(BIN_DIR)/$(BINARY) doctor

.PHONY: schema
schema: ## Capture a schema snapshot: make schema APIARY=../apiary/bin/apiary LABEL=0.18
	scripts/capture-schema.sh $(APIARY) $(LABEL)

.PHONY: docs
docs: ## Serve the documentation site locally
	mkdocs serve

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) site

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
