# Project-specific knobs for scripts/lint-walkthrough.py.
#
# The linter beside this file carries no project knowledge and is meant to stay
# byte-identical everywhere it lands. This file is the half you edit.

TOOL = "cs-vcr"
TOOL_PATH = "bin/cs-vcr"

DOCS = ["README.md", "INSTALL.md", "MANUAL.md", "SPEC.md", "CONTRIBUTING.md"]
EXTRA_DOCS = []

ENV_PREFIX = "CS_VCR_"

# cs-vcr also reads three unprefixed variables, VCR_LISTEN, VCR_ADMIN and
# VCR_ROOT, and MANUAL.md's Environment section carries all three.
ENV_INTERNAL = {}

# Read-only, offline, and safe to run in a checkout on every gate.
SAFE_VERBS = [
    "version",
    "config",
    "config claude",
    "config codex",
    "config opencode",
    "cassette ls",
    "cassette verify",
]

# A sample whose command is safe but whose output belongs to another machine.
SAMPLE_SKIP = {
    "cs-vcr config": "the sample is the defaults on a host with no config file,"
                     " and this checkout's home may carry one",
    "cs-vcr cassette ls": "the sample is a reader's own `build` cassette, not"
                          " the fixtures this repository commits",
}

PLACEHOLDER_OK = []

SOURCE_SKIP = {}

# goreleaser is optional here: `make build` falls back to `go build` without it,
# and INSTALL.md says so.
PREREQ_OK = ["goreleaser"]

AGENT_SECTION = "Notes for agents"

ALLOW = {}
