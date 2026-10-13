# Local lint entrypoint. Reads the same pin CI reads, so the two cannot drift.
GOLANGCI_LINT_VERSION := $(shell cat .golangci-lint-version)

.PHONY: lint
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...
