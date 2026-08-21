# cs-vcr — build/test/release.
# `make build` produces bin/cs-vcr via goreleaser (single host target,
# version-stamped, CGO_ENABLED=0). Falls back to plain `go build` if goreleaser
# is absent. See .goreleaser.yaml.

GORELEASER ?= goreleaser
CS_LINT    ?= cs-lint
BIN        := bin/cs-vcr
PKG        := ./cmd/cs-vcr
PREFIX     ?= $(HOME)/.local
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -s -w -X github.com/codesweep-ai/vcr/internal/cli.Version=$(VERSION)
# Tracked files where git knows them, every .go file where it does not — a
# fresh clone before its first commit has nothing tracked, and an empty list
# makes `gofmt -l` read stdin and hang rather than check anything.
GO_FILES   := $(shell git ls-files '*.go' 2>/dev/null | grep . || find . -name '*.go' -not -path './bin/*' -not -path './dist/*')

# Coverage is not a separate mode: every test target below writes Go binary
# coverage data into its own tier directory under $(COVERDIR), and `make
# coverage` merges whichever tiers are present. That is what lets
# `make test test-integration` report one aggregate number instead of the last
# tier overwriting the one before it. scripts/coverage.sh documents the layout.
# -test.gocoverdir must be absolute: `go test` runs each package's test binary
# with that package's directory as its working directory, so a relative path
# would scatter the data one directory per package.
# CS_COVERDIR, passed per tier below, tells a test that builds and execs the
# real binary where the instrumented child should write. It is not GOCOVERDIR
# because `go test` overwrites that one in the test process with a directory of
# its own, and does not fold what lands there back into the profile.
COVERDIR   ?= .coverage
COVER_ABS  := $(abspath $(COVERDIR))
# test/agents is the harness for the live tier, not shipped code, and it lives
# in ordinary .go files rather than _test.go ones -- so -coverpkg=./... would
# instrument 250-odd statements of test scaffolding and count them against the
# repo's coverage. Naming the packages excludes it. Recursive `=`, so `go list`
# runs only for the targets that use it.
COVERPKG    = $(shell go list ./... | grep -v '/test/' | paste -sd, -)
COVERFLAGS  = -covermode=atomic -coverpkg=$(COVERPKG)

.PHONY: help build build-go build-cover install uninstall test test-race fixtures fixtures-strict test-integration test-smoke coverage coverage-check coverage-baseline agent-versions vet fmt fmt-check check lint deadcode docs oss walkthrough cs-lint-installed ledger snapshot release release-check clean

.DEFAULT_GOAL := help

## help: list available targets (this menu)
help:
	@echo "cs-vcr make targets:"
	@grep -E '^## [a-z][a-z0-9-]*: ' $(MAKEFILE_LIST) | sed -E 's/^## ([^:]+): (.*)/  \1|\2/' | column -t -s '|'
	@echo ""
	@echo "  PREFIX=$(PREFIX) (install location; override with make install PREFIX=/usr/local)"

## build: host binary at bin/cs-vcr via goreleaser (single target)
build:
	@mkdir -p $(dir $(BIN))
	@if command -v $(GORELEASER) >/dev/null 2>&1; then \
		VERSION='$(VERSION)' $(GORELEASER) build --single-target --snapshot --clean --output $(BIN); \
	else \
		echo "goreleaser not found; using go build (run 'make build-go' explicitly to force)"; \
		$(MAKE) build-go; \
	fi

## build-cover: bin/cs-vcr built instrumented, for a gate that drives the real
## command rather than going through `go test`. Run it with GOCOVERDIR pointing
## at $(COVERDIR)/cli and what it executes joins the aggregate; CI's cassette
## job is the one that does. Without it that job proves the cassettes verify and
## measures nothing, which is how `cassette verify` came to read as uncovered
## while running on every push.
build-cover:
	@scripts/coverage.sh reset cli
	go build -cover $(COVERFLAGS) -o $(BIN) $(PKG)

## build-go: host binary via plain go build (no goreleaser needed)
build-go:
	@mkdir -p $(dir $(BIN))
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) $(PKG)

## install: copy bin/cs-vcr into $(PREFIX)/bin (default ~/.local/bin). A real
## copy, so the installed command keeps working if the checkout moves; re-run
## after a rebuild. Config lives in XDG dirs, so it runs from anywhere.
install: build
	@mkdir -p $(PREFIX)/bin
	install -m 0755 $(BIN) $(PREFIX)/bin/cs-vcr
	@echo "installed $(PREFIX)/bin/cs-vcr ($(VERSION))"
	@case ":$(PATH):" in *":$(PREFIX)/bin:"*) : ;; *) echo "note: add $(PREFIX)/bin to PATH" ;; esac

## uninstall: remove the installed binary
uninstall:
	rm -f $(PREFIX)/bin/cs-vcr

## test: unit tests
test:
	@scripts/coverage.sh reset unit
	CS_COVERDIR=$(COVER_ABS)/unit go test $(COVERFLAGS) ./... -args -test.gocoverdir=$(COVER_ABS)/unit

## test-race: the same suite under the race detector. The proxy serves several
## campaign members at once against one cassette, so a torn index or a lost
## entry is a real failure mode and not a theoretical one.
test-race:
	@scripts/coverage.sh reset race
	CS_COVERDIR=$(COVER_ABS)/race go test -race $(COVERFLAGS) ./... -args -test.gocoverdir=$(COVER_ABS)/race

## fixtures: record a cassette per agent/login combination in test/agents,
## scrub it, and replay it before keeping it. Calls real providers, so it costs
## money and needs the logins; every combination this host cannot sign in for is
## skipped with the reason. Local only — CI replays what this commits.
fixtures:
	CS_VCR_AGENTS_RECORD=1 go test ./test/agents -run TestRecordFixtures -v -timeout 60m -count=1

## fixtures-strict: the same recording, with a skip treated as a failure. For a
## host that holds every credential and means to re-record the whole matrix: a
## missing login skips with its reason under `fixtures`, and a run that recorded
## nothing reports the same green as one that recorded everything. Use
## `fixtures` while working with the subset this host can sign in for.
fixtures-strict:
	CS_VCR_AGENTS_RECORD=1 CS_VCR_AGENTS_STRICT=1 \
	  go test ./test/agents -run TestRecordFixtures -v -timeout 60m -count=1

## test-integration: the live tier — replay every committed fixture with the
## real agents, fabricated credentials and no provider configured or reachable.
## This is what CI runs. Needs claude, codex and opencode installed at the
## versions in test/agents/fixtures.json; anything missing is skipped, unless
## CS_VCR_AGENTS_STRICT=1 makes it fail.
test-integration:
	@scripts/coverage.sh reset integration
	CS_VCR_AGENTS=1 CS_COVERDIR=$(COVER_ABS)/integration \
	  go test $(COVERFLAGS) ./test/agents -run TestReplayFixtures -v -timeout 30m -count=1 \
	  -args -test.gocoverdir=$(COVER_ABS)/integration

## test-smoke: the profile to run before pushing — every committed fixture,
## replayed by the agent that recorded it, in about twenty seconds.
##
## The whole matrix rather than a chosen few. A subset has to be re-chosen every
## time a scenario is added, and the combination nobody remembers to add is the
## one that goes untested: the profile stays green while what it stopped
## covering rots. Twenty seconds does not need rationing.
##
## SMOKE_SCENARIOS narrows the run while working on one scenario, as a `|`
## alternation of names: `make test-smoke SMOKE_SCENARIOS=codex-chatgpt`. Empty
## is the default and means all of them, so no combination can go missing by
## being left out of a list.
SMOKE_SCENARIOS ?=

# Joined into one -run alternation. A backslash continuation becomes a space in
# make, so the spaces are substituted out here.
empty :=
space := $(empty) $(empty)
SMOKE_RUN := $(subst $(space),|,$(strip $(SMOKE_SCENARIOS)))
# No names means no subtest filter at all, which is what runs the whole matrix.
SMOKE_TARGET := TestReplayFixtures$(if $(SMOKE_RUN),/($(SMOKE_RUN)))

test-smoke:
	@scripts/coverage.sh reset smoke
	CS_VCR_AGENTS=1 CS_COVERDIR=$(COVER_ABS)/smoke \
	  go test $(COVERFLAGS) ./test/agents -run '$(SMOKE_TARGET)' -v -timeout 10m -count=1 \
	  -args -test.gocoverdir=$(COVER_ABS)/smoke

## agent-versions: the agent versions the committed fixtures were recorded with,
## as `name version` lines. What a CI job installs.
agent-versions:
	@python3 -c "import json;d=json.load(open('test/agents/fixtures.json'))['fixtures'];\
	print('\n'.join(sorted({'%s %s' % (f['agent'], f['agent_version']) for f in d.values()})))"

## coverage: merge every tier present under $(COVERDIR) and print the report
coverage:
	@scripts/coverage.sh report

## coverage-check: report, then fail if a package .coverage-baseline records as
## covered has stopped being reached. It checks presence, never a percentage:
## what it exists to catch is a suite that quietly stopped running.
coverage-check: coverage
	@scripts/coverage.sh check

## coverage-baseline: re-record .coverage-baseline. Records every tier present
## by default; pass BASELINE_TIERS to restrict it to the tiers CI actually runs,
## e.g. `make coverage-baseline BASELINE_TIERS="unit race smoke"`. Recording a
## tier CI never runs commits a promise nothing keeps.
coverage-baseline:
	@scripts/coverage.sh baseline $(BASELINE_TIERS)

## vet / fmt / lint
vet:
	go vet ./...
fmt:
	@test -n "$(GO_FILES)" || { echo "no Go files"; exit 0; }; gofmt -w $(GO_FILES)
fmt-check:
	@test -n "$(GO_FILES)" || { echo "no Go files to check"; exit 0; }; \
	unformatted="$$(gofmt -l $(GO_FILES))"; \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not gofmt-formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
## docs: check the prose against the writing rules in CONTRIBUTING.md
docs: cs-lint-installed
	$(CS_LINT) docs

## oss: the rules this repo has to satisfy as a published project
oss: cs-lint-installed
	$(CS_LINT) oss

## walkthrough: check the docs against the binary, the code and the build
walkthrough: build cs-lint-installed
	$(CS_LINT) walkthrough

# The three targets above are one shared tool: github.com/codesweep-ai/lint.
# Its knobs for this repo live in .cs-lint.yaml, and `cs-lint <linter> --explain`
# says what each rule wants.
cs-lint-installed:
	@command -v $(CS_LINT) >/dev/null 2>&1 || { \
		echo "cs-lint is not installed: go install github.com/codesweep-ai/lint/cmd/cs-lint@latest" >&2; \
		exit 2; \
	}

## ledger: validate the issue records and prove ledger.html is current
ledger:
	@command -v cs-ledger >/dev/null 2>&1 || { \
		echo "cs-ledger is not installed: go install github.com/codesweep-ai/ledger/cmd/cs-ledger@latest" >&2; \
		exit 2; \
	}
	cs-ledger check ledger

## check: the full local gate — fmt, vet, the linters, and the suites
check: fmt-check vet lint deadcode test test-race coverage-check docs oss walkthrough

## lint: the Go rules from .golangci.yml (see that file for what is on and why)
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint is not installed; see https://golangci-lint.run/welcome/install/" >&2; \
		exit 2; \
	}
	golangci-lint run

## deadcode: functions no entry point reaches. golangci-lint's `unused` cannot
## see this — it reasons one package at a time, so a function whose only caller
## lives in another package looks used. Drop -test and it answers a second,
## softer thing: what only a test keeps alive.
deadcode:
	@command -v deadcode >/dev/null 2>&1 || { \
		echo "deadcode is not installed: go install golang.org/x/tools/cmd/deadcode@latest" >&2; \
		exit 2; \
	}
	@out="$$(deadcode -test ./...)"; \
	if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

## snapshot: local release dry-run into dist/ (all platforms, archives, checksums).
## Skips SBOM + cosign signing (those need cyclonedx-gomod + cosign; run in CI/release).
snapshot:
	VERSION='$(VERSION)' $(GORELEASER) release --snapshot --clean --skip=sbom,sign

## release: tagged release (needs a pushed git tag + credentials). For a full
## signed+SBOM release install: go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest and cosign.
release:
	$(GORELEASER) release --clean

## release-check: validate .goreleaser.yaml
release-check:
	$(GORELEASER) check

## clean: remove build output
clean:
	rm -rf bin dist $(COVERDIR)
