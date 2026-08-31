# cs-vcr — build/test/release.
# `make build` produces bin/cs-vcr via goreleaser (single host target,
# version-stamped, CGO_ENABLED=0). Falls back to plain `go build` if goreleaser
# is absent. See .goreleaser.yaml.

GORELEASER ?= goreleaser
CS_LINT    ?= go tool cs-lint
# The linters the gates shell out to, all pinned and all built from the module
# cache, so a fresh checkout runs `make check` with nothing installed by hand.
# deadcode and actionlint are `tool` directives in go.mod and run with `go tool`.
# golangci-lint is one in go.golangci.mod, which says at its head why it needs a
# module file of its own.
GOLANGCI   := bin/tools/golangci-lint
BIN        := bin/cs-vcr
PKG        := ./cmd/cs-vcr
PREFIX     ?= $(HOME)/.local
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -s -w
# Tracked files where git knows them, every .go file where it does not — a
# fresh clone before its first commit has nothing tracked, and an empty list
# makes `gofmt -l` read stdin and hang rather than check anything.
GO_FILES   := $(shell git ls-files '*.go' 2>/dev/null | grep . || find . -name '*.go' -not -path './bin/*' -not -path './dist/*')

# What $(BIN) is made of. It is a real target rather than a phony one, so make
# skips the build when the binary is already newer than every input — which is
# what stops `make install` from repeating the `make build` that just ran.
#
# `find` rather than $(GO_FILES): a source file that is new and not yet added to
# the index is still an input. $(GIT_DIR)/HEAD is one because the version is the
# VCS stamp Go embeds, so a commit changes the binary even when no source did.
# The embedded files are listed because //go:embed makes them compile-time
# inputs; add to the list when a new one is embedded.
GIT_DIR    := $(shell git rev-parse --git-dir 2>/dev/null)
EMBED_DEPS := MANUAL.md
# //go:embed inputs deliberately left out of $(EMBED_DEPS). Nothing belongs here
# yet; `make embed-check` allows exactly this list and nothing else.
EMBED_EXEMPT :=
BUILD_DEPS := $(shell find . \( -name bin -o -name dist -o -name node_modules -o -name .git \) -prune -o -name '*.go' -print) \
              go.mod go.sum .goreleaser.yaml Makefile $(EMBED_DEPS) $(wildcard $(GIT_DIR)/HEAD)

# Which target last wrote $(BIN). Timestamps cannot tell an instrumented binary
# from an ordinary one, and `build-cover` writes the same path, so without this
# a `make build` after it would find a file newer than every source and leave
# the instrumented one in place — including through `make install`.
#
# The signal is its timestamp: $(FLAVOUR) is newer than $(BIN) exactly when
# $(BIN) is not the ordinary build. So build-cover writes it AFTER the binary,
# and every target that produces the ordinary one writes it BEFORE. It is named
# as a prerequisite rather than pulled in with $(wildcard), which would fix the
# list at parse time and miss a flavour written later in the same invocation --
# `make build-cover build` did exactly that.
FLAVOUR    := bin/.flavour

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

.PHONY: help tidy-check embed-check build build-go build-cover install uninstall test test-race fixtures fixtures-strict test-integration test-smoke coverage coverage-check ci coverage-baseline agent-versions vet fmt fmt-check check lint deadcode actionlint prose refs oss surface ledger snapshot release release-check clean

.DEFAULT_GOAL := help

## help: list available targets (this menu)
help:
	@echo "cs-vcr make targets:"
	@grep -E '^## [a-z][a-z0-9-]*: ' $(MAKEFILE_LIST) | sed -E 's/^## ([^:]+): (.*)/  \1|\2/' | column -t -s '|'
	@echo ""
	@echo "  PREFIX=$(PREFIX) (install location; override with make install PREFIX=/usr/local)"

## build: host binary at bin/cs-vcr via goreleaser (single target)
##
## A phony alias for $(BIN), so the work sits on a file target and make can skip
## it. `make build install`, and an `install` after a build, then copy what is
## already there instead of building the same binary a second time.
##
## --skip=before, because .goreleaser.yaml's before hooks are `go mod tidy`,
## `go vet ./...` and `go test ./...`: release gates that `make check` runs in
## its own right, and that made every build pay for the whole suite and rewrite
## go.mod as a side effect. `make snapshot` and `make release` still run them.
build: $(BIN)

$(BIN): $(BUILD_DEPS) $(FLAVOUR)
	@mkdir -p $(dir $@)
	@echo ordinary > $(FLAVOUR) # before the build, so $(BIN) ends up the newer
	@if command -v $(GORELEASER) >/dev/null 2>&1; then \
		VERSION='$(VERSION)' $(GORELEASER) build --single-target --snapshot --clean --skip=before --output $@; \
	else \
		echo "goreleaser not found; using go build (run 'make build-go' explicitly to force)"; \
		$(MAKE) build-go; \
	fi

# Only ever runs when the record is missing: a tree built before it existed, or
# one where bin/ was removed by hand rather than by `make clean`.
$(FLAVOUR):
	@mkdir -p $(dir $@) && echo ordinary > $@

## build-cover: bin/cs-vcr built instrumented, for a gate that drives the real
## command rather than going through `go test`. Run it with GOCOVERDIR pointing
## at $(COVERDIR)/cli and what it executes joins the aggregate; CI's cassette
## job is the one that does. Without it that job proves the cassettes verify and
## measures nothing, which is how `cassette verify` came to read as uncovered
## while running on every push.
build-cover:
	@scripts/coverage.sh reset cli
	go build -cover $(COVERFLAGS) -o $(BIN) $(PKG)
	@echo cover > $(FLAVOUR) # after the build, so it is newer than $(BIN); see $(FLAVOUR) above

## build-go: host binary via plain go build (no goreleaser needed)
build-go:
	@mkdir -p $(dir $(BIN))
	@echo ordinary > $(FLAVOUR) # the same binary `build` makes; it is what `build` falls back to
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) $(PKG)

## versions: what this build is made of — this repo's binary, every pinned tool,
## the Go toolchain, and whether a workspace is overriding the go.mod pins. The
## binary answers for itself; every tool is read out of the module file that
## pins it, which is the one place a `go tool` run can get it from. It
## deliberately depends on nothing and runs from source: reporting a version
## must not trigger a build.
## -buildvcs=true because `go run` leaves out the VCS stamp by default, and that
## stamp is the version now that nothing injects one with -X.
.PHONY: versions
versions:
	@if out="$$(go run -buildvcs=true -ldflags '$(LDFLAGS)' $(PKG) version 2>&1)"; then \
		printf '%-14s %-42s %s\n' '$(notdir $(BIN))' "$$(printf '%s\n' "$$out" | awk 'NR==1{print $$2}')" 'this repo'; \
	else \
		printf '%-14s %s\n' '$(notdir $(BIN))' "FAILED — $$(printf '%s\n' "$$out" | head -1)"; \
	fi
	@ver='{{with .Module}}{{if .Replace}}{{.Replace.Path}}{{else if .Version}}{{.Version}}{{else}}{{.Dir}}{{end}}{{end}}'; \
	for t in $$(go list tool 2>/dev/null); do \
		v="$$(go list -f "$$ver" $$t 2>/dev/null)"; \
		printf '%-14s %s\n' "$$(basename $$t)" "$${v:-FAILED}"; \
	done; \
	for t in $$(GOWORK=off go list -modfile=go.golangci.mod tool 2>/dev/null); do \
		v="$$(GOWORK=off go list -modfile=go.golangci.mod -f "$$ver" $$t 2>/dev/null)"; \
		printf '%-14s %s\n' "$$(basename $$t)" "$${v:-FAILED}"; \
	done
	@printf '%-14s %s\n' 'go' "$$(go env GOVERSION)"
	@w="$$(go env GOWORK)"; \
	case "$$w" in \
		''|off) printf '%-14s %s\n' 'workspace' 'off — versions above are go.mod pins' ;; \
		*)      printf '%-14s %s\n' 'workspace' "$$w — local checkouts override the go.mod pins" ;; \
	esac

## repin: move every codesweep-ai tool pin to its branch tip, then report. Uses
## GOPROXY=direct because the module proxy caches branch resolution and `@main`
## can come back a commit behind origin/main. Uses GOWORK=off so this edits the
## recorded pins even while a workspace is serving local checkouts.
.PHONY: repin
repin:
	@tools="$$(go list tool 2>/dev/null | grep codesweep-ai || true)"; \
	if [ -z "$$tools" ]; then \
		echo "no codesweep-ai tools declared yet — add the first with:" >&2; \
		echo "  GOPROXY=direct go get -tool github.com/codesweep-ai/lint/cmd/cs-lint@main" >&2; \
		exit 1; \
	fi; \
	GOWORK=off GOPROXY=direct go get -tool $$(echo "$$tools" | sed 's|$$|@main|')
	@GOWORK=off go mod tidy
	@$(MAKE) versions

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

## tidy-check: go.mod and go.sum are what `go mod tidy` would write
##
## The build no longer runs `go mod tidy`. It used to, as a goreleaser before
## hook, so every `make build` rewrote the module files as a side effect and
## nothing ever reported the drift. This gate replaces it and is the stronger of
## the two: it says what moved instead of quietly absorbing it, and it puts the
## originals back before failing, so a red gate leaves the tree as it found it.
## GOWORK=off, so a workspace serving local checkouts cannot make an untidy
## go.mod look tidy.
tidy-check:
	@t="$$(mktemp -d)"; cp go.mod go.sum "$$t/"; \
	GOWORK=off go mod tidy || { cp "$$t/go.mod" go.mod; cp "$$t/go.sum" go.sum; rm -rf "$$t"; exit 1; }; \
	if cmp -s go.mod "$$t/go.mod" && cmp -s go.sum "$$t/go.sum"; then \
		rm -rf "$$t"; echo "tidy: go.mod and go.sum are what \`go mod tidy\` writes"; \
	else \
		echo "go.mod/go.sum are not tidy; \`go mod tidy\` would apply:" >&2; \
		diff -u "$$t/go.mod" go.mod >&2; diff -u "$$t/go.sum" go.sum >&2; \
		cp "$$t/go.mod" go.mod; cp "$$t/go.sum" go.sum; rm -rf "$$t"; \
		exit 1; \
	fi

## embed-check: every //go:embed input is a prerequisite of the binary
##
## $(EMBED_DEPS) is written by hand, and an embed added without a line there
## leaves make holding a binary it calls current while the bytes inside it have
## moved -- the one kind of staleness no other gate can see. `go list` resolves
## the patterns itself, so this compares against what the toolchain actually
## embeds rather than re-reading the directives and reimplementing their globs.
embed-check:
	@deps="$$(mktemp)"; embeds="$$(mktemp)"; raw="$$(mktemp)"; \
	printf '%s\n' $(patsubst ./%,%,$(BUILD_DEPS)) $(EMBED_EXEMPT) | LC_ALL=C sort -u >"$$deps"; \
	if ! go list -f '{{range .EmbedFiles}}{{$$.Dir}}/{{.}}{{"\n"}}{{end}}' ./... >"$$raw"; then \
		rm -f "$$deps" "$$embeds" "$$raw"; \
		echo "embed-check: go list failed, so the embed set is unknown" >&2; exit 1; \
	fi; \
	grep -v '/node_modules/' "$$raw" | sed "s|^$$PWD/||" | grep . | LC_ALL=C sort -u >"$$embeds"; \
	missing="$$(LC_ALL=C comm -23 "$$embeds" "$$deps")"; n="$$(wc -l <"$$embeds")"; \
	rm -f "$$deps" "$$embeds" "$$raw"; \
	if [ -n "$$missing" ]; then \
		echo "//go:embed reads these, and no prerequisite of $(BIN) covers them:" >&2; \
		printf '  %s\n' $$missing >&2; \
		echo "add each to EMBED_DEPS, or a change to one will not rebuild the binary" >&2; \
		exit 1; \
	fi; \
	echo "embed: all $$n //go:embed inputs are prerequisites of $(notdir $(BIN))"
## prose: check how this repository's documents are written
prose:
	$(CS_LINT) prose

## refs: check that everything the documents point at is there
refs:
	$(CS_LINT) refs

## oss: the rules this repo has to satisfy as a published project
oss:
	$(CS_LINT) oss

## surface: check the docs against the binary, the code and the build
surface: build
	$(CS_LINT) surface

# The four targets above are one shared tool: github.com/codesweep-ai/lint,
# pinned in go.mod and run with `go tool`, so the gates use the version this
# repo records rather than whatever a machine happens to have installed. `make
# repin` moves that pin. prose and refs ask for no binary and run first;
# surface reads the one build makes.
# Its knobs for this repo live in .cs-lint.yaml, and `cs-lint <linter> --explain`
# says what each rule wants.

## ledger: validate the issue records and prove ledger.html is current
ledger:
	go tool cs-ledger check ledger

## check: the full local gate — fmt, vet, the linters, and the suites
check: fmt-check tidy-check embed-check vet lint deadcode test test-race coverage-check prose refs oss surface

# say prints a heading above each gate, so a long run reads as a list rather
# than as a wall. Bold where a terminal is reading it and plain where a pipe
# is: `make ci > ci.log` should leave a log somebody can read. The escapes are
# the same ones scripts/check.sh uses in tracer, which is where the shape came
# from.
define say
@if [ -t 1 ]; then printf '\n\033[1m==> %s\033[0m\n' "$(1)"; else printf '\n==> %s\n' "$(1)"; fi
endef

## ci: every gate the CI workflow runs, on this machine
##
## One Linux leg of .github/workflows/ci.yml, in the order CI runs it, so a
## red build is something you can see before you push rather than after.
##
## The live tier runs without CS_VCR_AGENTS_STRICT, so an agent this host does
## not carry skips with its reason instead of failing. CI sets it, because a
## runner installs all three at the pinned versions.
ci:
	$(call say,the gate a contributor runs before pushing)
	@$(MAKE) --no-print-directory check
	$(call say,actionlint)
	@$(MAKE) --no-print-directory actionlint
	$(call say,build)
	@$(MAKE) --no-print-directory build
	$(call say,release manifest)
	@$(MAKE) --no-print-directory release-check
	$(call say,ledger)
	@$(MAKE) --no-print-directory ledger
	$(call say,coverage build)
	@$(MAKE) --no-print-directory build-cover
	$(call say,the committed cassettes)
	GOCOVERDIR=$(COVER_ABS)/cli ./bin/cs-vcr cassette verify
	GOCOVERDIR=$(COVER_ABS)/cli ./bin/cs-vcr cassette scrub
	@# CI installs the pinned agents from this list before the live tier. A
	@# laptop already has its own, so printing it is how you see a mismatch.
	$(call say,the pinned agent versions)
	@$(MAKE) --no-print-directory agent-versions
	$(call say,the live tier)
	@$(MAKE) --no-print-directory test-integration
	@# The cassette gates run against a coverage build, which prints a warning
	@# on every invocation when GOCOVERDIR is unset. CI throws its runner away
	@# and a laptop does not, so put the ordinary binary back. build-cover
	@# wrote $(BIN) too, and $(FLAVOUR) is what stops make calling the
	@# instrumented file up to date and skipping this.
	$(call say,the ordinary binary, back)
	@$(MAKE) --no-print-directory build
	@printf '\nci: every gate ran. Not reproduced here: build-test on macOS, and\n'
	@printf 'the coverage job, which merges tiers from separate runners.\n'

# Built rather than run with `go tool`, because -modfile is refused in workspace
# mode. The build is the only step that reads go.golangci.mod, so only the build
# turns the workspace off; the linter then runs with it back on, against the
# checkouts a workspace is there to serve. A rebuild costs about a fifth of a
# second once the binary is current, which is what lets it be a prerequisite
# rather than a step somebody remembers.
$(GOLANGCI): go.golangci.mod
	@mkdir -p $(@D)
	@GOWORK=off go build -modfile=go.golangci.mod -o $@ \
		github.com/golangci/golangci-lint/v2/cmd/golangci-lint

## lint: the Go rules from .golangci.yml (see that file for what is on and why)
lint: $(GOLANGCI)
	$(GOLANGCI) run

## deadcode: functions no entry point reaches. golangci-lint's `unused` cannot
## see this — it reasons one package at a time, so a function whose only caller
## lives in another package looks used. Drop -test and it answers a second,
## softer thing: what only a test keeps alive.
deadcode:
	@out="$$(go tool deadcode -test ./...)"; \
	if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

## actionlint: the workflow files, which the forge validates only by refusing to
## run them. Extra runner labels it does not know about go in .github/actionlint.yaml.
actionlint:
	go tool actionlint

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
