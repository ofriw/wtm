# Local lint entrypoint. Reads the same pin CI reads, so the two cannot drift.
GOLANGCI_LINT_VERSION := $(shell cat .golangci-lint-version)

# The estimate-cause single-writer invariants are enforced by an AST test (see
# cause_writer_guard_test.go): each cause may be passed to an addEstimated*/
# priceClass* sink only by its one owning function. Grepping for literal counts
# coupled to layout and missed indirection; the parser names the exact file and
# line of an offending use.
.PHONY: guard
guard:
	go test -run 'TestCauseWriterGuard' .

.PHONY: lint fmt
lint: guard
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

# Same pinned binary and config as lint, so `make fmt` fixes exactly what CI checks.
fmt:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) fmt ./...
