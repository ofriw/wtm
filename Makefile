# Local lint entrypoint. Reads the same pin CI reads, so the two cannot drift.
GOLANGCI_LINT_VERSION := $(shell cat .golangci-lint-version)

# causeSubscriptionOverage may be set only by estimatedZero in usage.go: a stray
# writer would stamp the overage caveat onto rows that never earned it. causeCacheWrite
# has the same single-writer invariant: only addEstimatedClass in rates.go sets it.
# A Go test reading source text would couple to layout, so these greps are the
# enforcement. Literal-occurrence counts replace call-paren counts, so an
# indirection (e.g. `cause := causeSubscriptionOverage`) cannot hide a writer.
# usage.go holds exactly one literal per site: the doc comment, the const
# declaration, the estimatedZero write, and the needsOverageLegend read. rates.go
# holds the one addEstimatedClass write and nothing else.
.PHONY: guard
guard:
	@offenders="$$(grep -rn --include='*.go' 'causeSubscriptionOverage' . \
		| grep -v '^\./usage\.go:' | grep -v '_test\.go:')"; \
	test -z "$$offenders" || { echo "$$offenders"; echo "causeSubscriptionOverage: only estimatedZero in usage.go may set it"; exit 1; }
	@in_usage="$$(grep -o 'causeSubscriptionOverage' usage.go | wc -l | tr -d ' ')"; \
	test "$$in_usage" = "4" || { echo "causeSubscriptionOverage: usage.go must hold exactly 4 literals (comment, declaration, estimatedZero write, needsOverageLegend read), found $$in_usage"; exit 1; }
	@cache_write="$$(grep -rn --include='*.go' -E '(addEstimated\(|addEstimatedClass\(|priceClass\()[^)]*causeCacheWrite' . \
		| grep -v '^\./rates\.go:' | grep -v '_test\.go:')"; \
	test -z "$$cache_write" || { echo "$$cache_write"; echo "causeCacheWrite: only addEstimatedClass in rates.go may set it"; exit 1; }
	@in_rates="$$(grep -o 'causeCacheWrite' rates.go | wc -l | tr -d ' ')"; \
	test "$$in_rates" = "1" || { echo "causeCacheWrite: rates.go must hold exactly 1 literal (the addEstimatedClass write), found $$in_rates"; exit 1; }

.PHONY: lint fmt
lint: guard
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

# Same pinned binary and config as lint, so `make fmt` fixes exactly what CI checks.
fmt:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) fmt ./...
