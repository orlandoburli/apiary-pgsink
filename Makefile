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

PG_CONTAINER := pgsink-dev
PG_PORT      := 55432
PG_DSN       := postgres://pgsink:pgsink@localhost:$(PG_PORT)/pgsink

.PHONY: test
test: ## Run all tests (PostgreSQL tests skip unless PGSINK_TEST_DSN is set)
	go test ./...

.PHONY: test-all
test-all: pg-up ## Run all tests including the PostgreSQL integration tests
	PGSINK_TEST_DSN="$(PG_DSN)" go test ./...

.PHONY: pg-up
pg-up: ## Start a throwaway PostgreSQL for the integration tests
	@docker inspect $(PG_CONTAINER) >/dev/null 2>&1 || \
	  docker run -d --name $(PG_CONTAINER) \
	    -e POSTGRES_USER=pgsink -e POSTGRES_PASSWORD=pgsink -e POSTGRES_DB=pgsink \
	    -p $(PG_PORT):5432 postgres:17-alpine >/dev/null
	@docker start $(PG_CONTAINER) >/dev/null 2>&1 || true
	@for i in $$(seq 1 30); do \
	  docker exec $(PG_CONTAINER) pg_isready -U pgsink -q 2>/dev/null && break; \
	  sleep 1; \
	done
	@echo "PGSINK_TEST_DSN=$(PG_DSN)"

.PHONY: pg-down
pg-down: ## Remove the throwaway PostgreSQL
	-docker rm -f $(PG_CONTAINER) >/dev/null 2>&1
	@echo "removed $(PG_CONTAINER)"

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

.PHONY: docker
docker: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t apiary-pgsink:$(VERSION) .

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) site

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
