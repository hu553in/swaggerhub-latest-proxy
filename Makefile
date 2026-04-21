BUILD_DIR ?= ./dist

GOLANGCI_LINT_CONFIG_URL ?= https://raw.githubusercontent.com/maratori/golangci-lint-config/refs/heads/main/.golangci.yml

.PHONY: ensure-build-dir
ensure-build-dir:
	mkdir -p $(BUILD_DIR)

.PHONY: pre-commit
pre-commit: build lint check-deps

.PHONY: check
check: build fmt lint check-deps

.PHONY: install-deps
install-deps:
	go mod download

.PHONY: update-lint-config
update-lint-config:
	@tmp=$$(mktemp); \
	if curl -fsSL $(GOLANGCI_LINT_CONFIG_URL) -o "$$tmp"; then \
		mv "$$tmp" .golangci.yaml && \
		sed -i '' "s|github.com/my/project|github.com/hu553in/swaggerhub-latest-proxy|g" .golangci.yaml; \
	else \
		rm -f "$$tmp"; \
		exit 1; \
	fi

.PHONY: fmt
fmt:
	golangci-lint fmt

.PHONY: lint
lint:
	golangci-lint run

.PHONY: check-deps
check-deps: install-deps
	go tool govulncheck ./...

.PHONY: build
build: install-deps
	CGO_ENABLED=0 GOFLAGS="-buildvcs=false" \
    go build -trimpath -ldflags="-s -w" -o $(BUILD_DIR)/swaggerhub-latest-proxy ./cmd/swaggerhub-latest-proxy

.PHONY: clean
clean:
	rm -rf $(BUILD_DIR)
