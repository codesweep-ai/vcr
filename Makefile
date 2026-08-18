# cs-vcr — build/test/release.
# `make build` produces bin/cs-vcr via goreleaser (single host target,
# version-stamped, CGO_ENABLED=0). Falls back to plain `go build` if goreleaser
# is absent. See .goreleaser.yaml.

GORELEASER ?= goreleaser
BIN        := bin/cs-vcr
PKG        := ./cmd/cs-vcr
PREFIX     ?= $(HOME)/.local
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -s -w -X github.com/codesweep-ai/vcr/internal/cli.Version=$(VERSION)
# Tracked files where git knows them, every .go file where it does not — a
# fresh clone before its first commit has nothing tracked, and an empty list
# makes `gofmt -l` read stdin and hang rather than check anything.
GO_FILES   := $(shell git ls-files '*.go' 2>/dev/null | grep . || find . -name '*.go' -not -path './bin/*' -not -path './dist/*')

.PHONY: help build build-go install uninstall test test-race fixtures test-integration test-smoke agent-versions vet fmt fmt-check check lint deadcode docs oss ledger snapshot release release-check clean

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
		$(GORELEASER) build --single-target --snapshot --clean --output $(BIN); \
	else \
		echo "goreleaser not found; using go build (run 'make build-go' explicitly to force)"; \
		$(MAKE) build-go; \
	fi

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
	go test ./...

## test-race: the same suite under the race detector. The proxy serves several
## campaign members at once against one cassette, so a torn index or a lost
## entry is a real failure mode and not a theoretical one.
test-race:
	go test -race ./...

## fixtures: record a cassette per agent/login combination in test/agents,
## scrub it, and replay it before keeping it. Calls real providers, so it costs
## money and needs the logins; every combination this host cannot sign in for is
## skipped with the reason. Local only — CI replays what this commits.
fixtures:
	CS_VCR_AGENTS_RECORD=1 go test ./test/agents -run TestRecordFixtures -v -timeout 60m -count=1

## test-integration: the live tier — replay every committed fixture with the
## real agents, fabricated credentials and no provider configured or reachable.
## This is what CI runs. Needs claude, codex and opencode installed at the
## versions in test/agents/fixtures.json; anything missing is skipped, unless
## CS_VCR_AGENTS_STRICT=1 makes it fail.
test-integration:
	CS_VCR_AGENTS=1 go test ./test/agents -run TestReplayFixtures -v -timeout 30m -count=1

## test-smoke: the profile to run before pushing — one scenario per agent, and
## between them all three surfaces cs-vcr routes, in about five seconds. A
## subset of test-integration, which stays cheap enough for CI to run whole.
##
## The members are spelled out rather than picked by rule: a profile that took
## the first three scenarios would stop covering a surface the day one is
## renamed, and say nothing about it.
SMOKE_SCENARIOS ?= claude-code-subscription \
                   codex-api-key \
                   opencode-fireworks

# Joined into one -run alternation. A backslash continuation becomes a space in
# make, so the spaces are substituted out here.
empty :=
space := $(empty) $(empty)
SMOKE_RUN := $(subst $(space),|,$(strip $(SMOKE_SCENARIOS)))

test-smoke:
	CS_VCR_AGENTS=1 go test ./test/agents -run 'TestReplayFixtures/($(SMOKE_RUN))' -v -timeout 10m -count=1

## agent-versions: the agent versions the committed fixtures were recorded with,
## as `name version` lines. What a CI job installs.
agent-versions:
	@python3 -c "import json;d=json.load(open('test/agents/fixtures.json'))['fixtures'];\
	print('\n'.join(sorted({'%s %s' % (f['agent'], f['agent_version']) for f in d.values()})))"

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
docs:
	python3 scripts/lint-docs.py

## oss: the rules this repo has to satisfy as a published project
oss:
	python3 scripts/lint-oss.py

## walkthrough: check the docs against the binary, the code and the build
walkthrough: build-go
	python3 scripts/lint-walkthrough.py

## ledger: validate the issue records and prove ledger.html is current
ledger:
	@command -v cs-ledger >/dev/null 2>&1 || { \
		echo "cs-ledger is not installed: go install github.com/codesweep-ai/ledger/cmd/cs-ledger@latest" >&2; \
		exit 2; \
	}
	cs-ledger check ledger

## check: the full local gate — fmt, vet, the linters, and the suites
check: fmt-check vet lint deadcode test test-race docs oss walkthrough

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
	$(GORELEASER) release --snapshot --clean --skip=sbom,sign

## release: tagged release (needs a pushed git tag + credentials). For a full
## signed+SBOM release install: go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest and cosign.
release:
	$(GORELEASER) release --clean

## release-check: validate .goreleaser.yaml
release-check:
	$(GORELEASER) check

## clean: remove build output
clean:
	rm -rf bin dist
