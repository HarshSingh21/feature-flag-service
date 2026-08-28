# Makefile - feature-flag-service
#
# `make` with no target prints this file's self-documenting help.
# `make ci` runs exactly the gates CI runs, so a green local run means a green PR.

GO          ?= go
BIN_DIR     := bin
BINARY      := $(BIN_DIR)/flagd
CMD         := ./cmd/flagd
PKGS        := ./...
COVERFILE   := coverage.txt
COVERHTML   := coverage.html

.DEFAULT_GOAL := help

.PHONY: help build test test-short cover bench lint fmt fmt-check vet tidy run clean ci

help: ## Print this help
	@echo "feature-flag-service - available targets:"
	@echo ""
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
	    | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
	@echo ""

build: ## Compile the flagd binary into bin/
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -o $(BINARY) $(CMD)

test: ## Full test suite with the race detector (the merge gate)
	$(GO) test -race -count=1 $(PKGS)

test-short: ## Fast feedback loop - skips long-running and distribution tests
	$(GO) test -short -count=1 $(PKGS)

cover: ## Race-enabled coverage run, then open an HTML report
	$(GO) test -race -covermode=atomic -coverprofile=$(COVERFILE) $(PKGS)
	$(GO) tool cover -html=$(COVERFILE) -o $(COVERHTML)
	@echo "coverage report written to $(COVERHTML)"
	@$(GO) tool cover -func=$(COVERFILE) | tail -n 1

bench: ## Run benchmarks only, with allocation counts
	$(GO) test -run '^$$' -bench . -benchmem $(PKGS)

lint: ## Run golangci-lint (advisory today, see CONTRIBUTING.md)
	@command -v golangci-lint >/dev/null 2>&1 || { \
	    echo "golangci-lint not installed - see https://golangci-lint.run/welcome/install/"; \
	    exit 1; \
	}
	golangci-lint run

fmt: ## Rewrite all Go files with gofmt
	gofmt -s -w .

fmt-check: ## Fail if any file is not gofmt-clean (gofmt itself always exits 0)
	@out="$$(gofmt -s -l . 2>&1)"; \
	if [ -n "$$out" ]; then \
	    echo "these files are not gofmt-clean:"; \
	    echo "$$out"; \
	    echo "run: make fmt"; \
	    exit 1; \
	fi
	@echo "gofmt: clean"

vet: ## Run go vet
	$(GO) vet $(PKGS)

tidy: ## Tidy go.mod and go.sum
	$(GO) mod tidy

run: ## Run flagd from source
	$(GO) run $(CMD)

clean: ## Remove build output and coverage artefacts
	rm -rf $(BIN_DIR) $(COVERFILE) $(COVERHTML)

ci: fmt-check vet build test ## Everything CI enforces, in CI's order
	@echo ""
	@echo "all gates passed"
