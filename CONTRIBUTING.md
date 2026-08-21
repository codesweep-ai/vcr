# Contributing to cs-vcr

These rules apply to **humans and coding agents alike**. If you are an agent working in this repo,
read this file before you change anything and follow it.

Bug reports and pull requests are welcome. For a security issue, use GitHub's private
vulnerability reporting on this repository's Security tab, rather than opening a public issue.

## How a change gets in

File a bug or an idea as a GitHub issue on this repository. For a fix that stands on its own, a pull
request on its own is enough. For anything that changes behaviour a user can see, open an issue
first, so the design gets settled before you write it.

1. Fork the repository, and create a branch off `main`.
2. Make the change, with its test.
3. Run `make check`, which is the same gate CI runs.
4. Open a pull request against `main`, and say what the change does and why.

Review asks four questions. Does the change hold the invariants below? Does a test fail without it?
Does every user-visible change land in exactly one document? Does the history read the way this file
describes? Expect comments rather than silence, and expect a small change to move quickly.

By opening a pull request you agree that your contribution ships under the
[Apache 2.0 licence](LICENSE) this project is released under.

## Before you push

```bash
make check        # gofmt, vet, unit tests, the race detector and every linter
make test-smoke   # three real agents replaying every committed cassette, about 20s
```

`make check` shells out to three tools that do not come with Go, and the ledger below needs a
fourth. Install all four once, pinning `golangci-lint` to the version CI runs:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
go install golang.org/x/tools/cmd/deadcode@latest
go install github.com/codesweep-ai/lint/cmd/cs-lint@latest
go install github.com/codesweep-ai/ledger/cmd/cs-ledger@latest
```

`cs-lint` is deliberately not pinned. It is this family's own linter, and CI installs it from source
the same way, so a check it gains reaches you on the day it lands.

This repo keeps a **ledger** of open issues in `ledger/`. Read
[`ledger/AGENTS.md`](ledger/AGENTS.md) before you start work, and follow it as you go. A commit
that touches `ledger/` needs `cs-ledger render && cs-ledger check` to pass first, and `make ledger`
runs the check half.

## The one thing a change must not break

**Replay must not be able to spend money.** It never falls through to upstream, however it is
configured. That is why the command that built the server sets `ReachesUpstream`, rather than each
call site branching on it. `TestReplayModeNeverContactsUpstream` asserts it against an upstream
that fails the test if it is dialled at all, and `TestReachesUpstreamMatchesWhatTheServerDoes`
holds the bit itself honest. A change that lets a miss reach a provider "just this once" turns a $0
CI bill into a surprise invoice, and will be rejected however convenient it is.

cs-vcr also does not authenticate callers, hold credentials, swap keys or redact anything. A patch
adding any of those is adding a second product. Request headers are never recorded, which is what
makes "no redaction" safe rather than reckless, so keep it that way.

## Tests are part of the change

Every behavior change ships with test coverage. A change with no test is only acceptable when the
behavior genuinely cannot be observed in a test. Say so in the pull request.

- `make test` is the tier a change usually needs. Everything cs-vcr does is observable from a Go
  test: the cobra tree driven with a fake environment, the proxy driven against a local upstream, a
  cassette in a temp dir. That is where the costly-if-silently-wrong things live, such as the
  record/replay round trip and SSE framing.
- `make test-smoke` replays the committed cassettes in about twenty seconds, and is the one to run
  before every push. `SMOKE_SCENARIOS=<name>` narrows it while you work on a single scenario.
- `make test-integration` replays the same matrix against the real clients. Run it after touching
  normalization, the SSE handling or the cassette format, which are the things a hand-written
  fixture cannot check.
- **Every gate fires with both a positive and a negative test.** The negative half is usually the
  one that matters: that an unknown replay refuses *and did not dial*, or that a miss *fails the
  session* rather than passing quietly.
- Test the contract, not the implementation: the exit code, the bytes that reached the client, the
  header that arrived upstream. Say *why* the case matters in a comment when it isn't obvious.

[`SPEC.md`](SPEC.md) holds what each tier proves, the scenario matrix, and what recording and
replaying a cassette require. Read it before you add a scenario or re-record one.

### Coverage

Every test target writes coverage into its own tier under `.coverage/`, so running several
aggregates rather than overwrites. `make coverage` merges what is there and prints the report.

`make coverage-check` runs inside `make check` and in CI. It fails when a package
`.coverage-baseline` lists stops being reached: presence, not a percentage. What it catches is a
suite that stopped running while the tests still report green. When a package is meant to lose its
coverage, rerun `make coverage-baseline` and commit the result.

The live and cassette tiers build `cs-vcr` with `-cover`, so what the real binary runs counts too.
`test/agents` is harness, so it stays out of `-coverpkg`.

## Cassettes

Cassettes are committed and reviewed, which is what the directory-per-cassette format is for. When
a change alters recorded traffic, the diff has to be legible in the PR. If it is not, fix the
format rather than asking the reviewer to cope.

`cassette verify` gates merges. A change to the normalization ruleset means bumping
`normalize.version` and re-recording, in the same PR.

## Commits

Keep one idea per commit. If a change will not fit that shape, it is doing more than one thing, so
split it.

**Subject**, always. Under 60 characters, imperative, no trailing period, completing *"If applied,
this commit will …"*. Say what the change does, in plain English rather than in this project's
internal shorthand. Use no conventional-commit prefix: `fix(proxy):` names a category rather than a
change, and the category is already in the diff.

**Body**, only when the subject leaves a real question a reader would otherwise have to open the
diff to answer. Write the answer in plain English, in whole sentences, addressed to somebody who was
not there. Wrap it at 72 columns. Most commits need no body at all.

Say what the change does and what constrained it. Leave out how the work was scheduled, how it was
tested, and what prompted it. A rule's reason belongs beside the rule in [`SPEC.md`](SPEC.md), and
the investigation that found it belongs in the pull request.

Where a body carries more than one independent point, one line each reads better than a paragraph.
Never reach for another point to fill the shape. A line that restates the subject in different words
is worse than no body, and a body written to a length is the commonest way a message stops being
read.

```
Fix the port parse in the sidecar redirect rule
```

```
Replay SSE per event, not as one write

A client assembling deltas sees framing rather than bytes, so
a single write plays back as a session the client cannot read.
```

```
Match a value split across two events

- A delta can break a token in half; the matcher joined them.
- Empty items are ignored, so a keepalive cannot shift a match.
```

Keep the `Co-Authored-By:` trailer when an agent wrote the change. Drop any trailer linking to the
agent's session or transcript. Such a link is private to whoever ran it and dead to everyone else,
and it cannot be fixed after publication.

## Docs

Behavior a user can see belongs in the docs next to it, updated in the same commit as the code.
`README.md` is the tour, `INSTALL.md` says how to get the binary, `MANUAL.md` is the command
reference, and `SPEC.md` says what cs-vcr guarantees and how it is built. A change to a **MUST** in `SPEC.md` changes the contract, so say so
in the PR.

## Writing

Six principles carry the voice. Read them before you write a document, and apply them when you edit
one:

1. **Introduce a term where you first use it**, in the same sentence, or link to the page that
   defines it. A reader should never meet a word the docs have not explained.
2. **State the point first, then qualify it.** Opening with the qualifier makes the reader decode
   the sentence backwards.
3. **Give every sentence a subject and a verb.** "Two version numbers, one verdict, one remedy"
   reads as knowing rather than clear. Say what the thing is.
4. **A walkthrough is steps that work.** Put the reasons somewhere else. A reader working through
   one wants commands that run.
5. **Describe what the software does, not how it came to do it.** Leave out what the project used
   to do, what was tried and dropped, and numbers from a run somebody did once.
6. **Do not explain a design by contrast with a worse one.** Say what it is and what you get,
   rather than asking the reader to picture a design nobody proposed.

The mechanical rules are enforced rather than restated here.
[`cs-lint`](https://github.com/codesweep-ai/lint) carries them, and `make check` runs all three of
its linters over this repository:

| Command | Target | What it checks |
|---|---|---|
| `cs-lint docs` | `make docs` | How the documents are written. |
| `cs-lint oss` | `make oss` | What this repository owes a reader as a published project. |
| `cs-lint walkthrough` | `make walkthrough` | Whether the documents still describe the software. |

`--explain` prints what each rule wants and the guidance behind it:

```bash
cs-lint docs --explain
```

That listing is the authority. Where this section and the linter disagree, the linter is right and
this section is a bug. Every knob lives in [`.cs-lint.yaml`](.cs-lint.yaml), and a check that
reports noise is a check to fix rather than a report to work around.

A check turned off here is a waiver, written under `allow` as an identifier and the reason it was
traded away. The reason is required, and it is printed with the finding, because a waiver nobody can
review is a rule deleted in private.

**What not to change.** This project's voice is a strength: concrete, opinionated, free of
marketing padding. These rules are about mechanics. Where one of them fights the voice, the voice
wins, and the exception is worth a sentence in the pull request.

## AI-assisted contributions

An agent wrote most of this repository, and you are welcome to use one. The standard is the same
either way: you are responsible for what you submit.

Point your tool at [`AGENTS.md`](AGENTS.md), which routes it to the documents that hold the
conventions, and check three things before you open the pull request:

- You understand every line, and can answer a question about it without going back to the tool.
- You ran `make check` and it passed.
- You cut what the tool added to fill space. A model pads a commit body to the shape it was shown,
  and comments that restate the code around them. Both read as noise to a maintainer, and both are
  yours to remove.

Keep the `Co-Authored-By:` trailer, which is how the work is disclosed. An unattended agent must not
open pull requests or comment on this repository.
