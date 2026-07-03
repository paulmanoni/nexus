# nexus — developer + CI tasks.
#
# The repo is three Go modules: the main module plus two opt-in router/DI
# adapters that version independently (gin, fx) and `replace` the parent
# locally. `test`/`vet` iterate all three so an adapter can't silently break.
#
#   make test           # go test -race across every module
#   make vet            # go vet across every module
#   make fmt            # gofmt -w the whole tree
#   make fmt-check      # fail if any file needs gofmt (CI gate)
#   make lint           # golangci-lint across every module
#   make cover          # coverage profile + per-func report (main module)
#   make cover-check    # fail if main-module coverage drops below COVER_MIN
#   make generate-check # CI drift gate for committed //@ handler codegen
#   make golden-update  # regenerate golden files after an intentional change
#   make ci             # everything CI runs

# All Go modules in the repo (dir containing a go.mod).
MODULES := . di/fxcontainer httpx/ginrouter

# Pinned golangci-lint version — keep in sync with .github/workflows/ci.yml
# so `make lint` and CI enforce the exact same linters (config: .golangci.yml).
GOLANGCI_VERSION ?= v1.64.8

# Minimum acceptable statement coverage for the main module (percent). A ratchet
# against regression, not a target — raise it as coverage climbs.
COVER_MIN ?= 45

# gin prints router debug noise unless told it's in release mode.
export GIN_MODE := release

.PHONY: test vet fmt fmt-check lint cover cover-check generate-check golden-update ci tidy

test:
	@for m in $(MODULES); do \
		echo "==> go test -race ($$m)"; \
		( cd $$m && go test -race -count=1 ./... ) || exit 1; \
	done

vet:
	@for m in $(MODULES); do \
		echo "==> go vet ($$m)"; \
		( cd $$m && go vet ./... ) || exit 1; \
	done

# gofmt the whole tree (submodule dirs are under the root, so one pass covers
# every module).
fmt:
	gofmt -w .

# CI gate: fail if any tracked .go file isn't gofmt-clean. `gofmt -l .` lists
# offenders; a non-empty list is the failure.
fmt-check:
	@offenders=$$(gofmt -l .); \
	if [ -n "$$offenders" ]; then \
		echo "FAIL: these files need gofmt (run: make fmt):"; \
		echo "$$offenders"; \
		exit 1; \
	fi; \
	echo "gofmt: clean"

# golangci-lint across every module, pinned + config-driven (.golangci.yml).
# The config auto-resolves from the repo root for submodules, but we pass it
# explicitly so the version and rule set are unambiguous.
lint:
	@for m in $(MODULES); do \
		echo "==> golangci-lint ($$m)"; \
		( cd $$m && go run github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_VERSION) run --config $(CURDIR)/.golangci.yml ./... ) || exit 1; \
	done

cover:
	go test -covermode=atomic -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

cover-check:
	@go test -covermode=atomic -coverprofile=coverage.out ./... >/dev/null
	@total=$$(go tool cover -func=coverage.out | awk '/^total:/ {gsub(/%/,"",$$3); print $$3}'); \
	echo "main-module coverage: $$total% (floor $(COVER_MIN)%)"; \
	awk -v t=$$total -v min=$(COVER_MIN) 'BEGIN { if (t+0 < min+0) { printf "FAIL: coverage %.1f%% < floor %d%%\n", t, min; exit 1 } }'

# Drift gate: the committed *_gen.go for //@-annotated handlers must match a fresh
# regeneration. Builds the CLI, then --check (no writes; non-zero on drift) in
# every tree that ships committed generated handlers.
generate-check:
	go build -o bin/nexus ./cmd/nexus
	@cd examples/notes && ../../bin/nexus generate handlers --check ./...

golden-update:
	UPDATE_GOLDEN=1 go test ./client/... ./internal/handlergen/... ./registry/... -run Golden -count=1

tidy:
	@for m in $(MODULES); do \
		echo "==> go mod tidy ($$m)"; \
		( cd $$m && go mod tidy ) || exit 1; \
	done

ci: fmt-check vet lint test cover-check generate-check
