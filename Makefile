# Local lint entrypoint. Reads the same pin CI reads, so the two cannot drift.
GOLANGCI_LINT_VERSION := $(shell cat .golangci-lint-version)

# causeSubscriptionOverage may be set only by estimatedZero in usage.go: a stray
# writer would stamp the overage caveat onto rows that never earned it. A Go test
# reading source text would couple to layout, so this grep is the enforcement.
.PHONY: guard
guard:
	@offenders="$$(grep -rn --include='*.go' 'causeSubscriptionOverage' . \
		| grep -v '^\./usage\.go:' | grep -v '_test\.go:')"; \
	test -z "$$offenders" || { echo "$$offenders"; echo "causeSubscriptionOverage: only estimatedZero in usage.go may set it"; exit 1; }

.PHONY: lint fmt
lint: guard
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

# Same pinned binary and config as lint, so `make fmt` fixes exactly what CI checks.
fmt:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) fmt ./...
