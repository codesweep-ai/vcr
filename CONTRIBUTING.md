# Contributing to cs-vcr

These rules apply to **humans and coding agents alike**. If you are an agent working in this repo,
read this file before you change anything and follow it.

Bug reports and pull requests are welcome. For a security issue, use GitHub's private
vulnerability reporting on this repository's Security tab, rather than opening a public issue.

## Submitting a change

File a bug or an idea as a GitHub issue on this repository. For a fix that stands on its own, a pull
request on its own is enough. For anything that changes behaviour a user can see, open an issue
first, so the design gets settled before you write it.

1. Fork the repository, and create a branch off `main`.
2. Make the change, with its test.
3. Run `make check`, which is the same gate CI runs.
4. Open a pull request against `main`, and say what the change does and why.

Expect comments rather than silence, and expect a small change to move quickly. A reviewer asks
whether the change keeps the design rules below, whether a test fails without it, and where a reader
would find it documented.

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
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1
go install golang.org/x/tools/cmd/deadcode@latest
go install github.com/codesweep-ai/lint/cmd/cs-lint@latest
go install github.com/codesweep-ai/ledger/cmd/cs-ledger@latest
```

`cs-lint` is not pinned. CI installs it from source the same way you do, so a check it gains reaches
you on the day it lands.

This repo keeps a **ledger** of open issues in `ledger/`. Read
[`ledger/AGENTS.md`](ledger/AGENTS.md) before you start work, and follow it as you go. A commit
that touches `ledger/` needs `cs-ledger render && cs-ledger check` to pass first, and `make ledger`
runs the check half.

## Design rules

Your change has to keep these. Each one names the test or the review that holds it.

**Replay must not be able to spend money.** It never falls through to upstream, however it is
configured. That is why the command that built the server sets `ReachesUpstream`, rather than each
call site branching on it. `TestReplayModeNeverContactsUpstream` asserts it against an upstream
that fails the test if it is dialled at all, and `TestReachesUpstreamMatchesWhatTheServerDoes`
holds the bit itself honest. A change that lets a miss reach a provider "just this once" turns a $0
CI bill into a surprise invoice, and will be rejected however convenient it is.

cs-vcr also does not authenticate callers, hold credentials, swap keys or redact anything. A patch
adding any of those is adding a second product. Request headers are never recorded, which is what
makes "no redaction" safe rather than reckless, so keep it that way.

## Tests

Ship a test with your change. Where a behaviour genuinely cannot be observed in a test, say so in
the pull request.

`make test` is the tier a change usually needs, because everything cs-vcr does is observable from a
Go test. Run `make test-smoke` before every push: it replays the committed cassettes in about twenty
seconds, and `SMOKE_SCENARIOS=<name>` narrows it while you work on one scenario. After touching
normalization, the SSE handling or the cassette format, run `make test-integration`, because those
are the things a hand-written fixture cannot check.

Test the contract, not the implementation: the exit code, the bytes that reached the client, the
header that arrived upstream. Test what happens when it fails, not only when it works. Say why the
case matters in a comment when it is not obvious.

Never lower a coverage baseline to make a run green. [`SPEC.md`](SPEC.md#115-testing) holds what
each tier proves, the scenario matrix, what recording and replaying a cassette require, and how
coverage is measured. Read it before you add a scenario or re-record one.

## Cassettes

Cassettes are committed and reviewed, which is what the directory-per-cassette format is for. When
a change alters recorded traffic, the diff has to be legible in the PR. If it is not, fix the
format rather than asking the reviewer to cope.

`make fixtures` records what this host can sign in for and skips the rest with the reason.
`make fixtures-strict` fails on a skip instead, which is what a host holding every credential
wants. `scripts/record-fixtures.sh` sets the environment for both and checks it first.

Recording asks each agent for one word against its real provider before it opens a cassette, so a
revoked key costs nothing and leaves the committed fixture where it was.

`cassette verify` gates merges. A change to the normalization ruleset means bumping
`normalize.version` and re-recording, in the same PR.

## Commits

**Keep it short.** One idea per commit, and a message a reader takes in at a glance. If a change
will not fit one idea, split it.

**Subject**, always. Under 60 characters, imperative, no trailing period, completing *"If applied,
this commit will …"*. Say what the change does, in plain English rather than in this project's
internal shorthand. Use no category label: `fix(proxy):`, `bugfix:` and `[docs]` each name a class
of change rather than the change itself, which the diff already shows. The gate fails on one, so
amend before you push.

**Body**, rarely. Most commits need none. Add one only when the subject leaves a question a reader
would otherwise have to open the diff to answer, and then answer that question. A sentence or two
does it. Wrap it at 72 columns.

Leave out how the work was scheduled, how you tested it, and what led you to it, and stop once the
question is answered. A second paragraph usually means the message has turned into a report of the
session. A rule's reason belongs beside the rule in [`SPEC.md`](SPEC.md), and the investigation that
found it belongs in the pull request.

```
Fix the port parse in the sidecar redirect rule
```

```
Replay SSE per event, not as one write

A client assembling deltas sees framing rather than bytes, so
a single write plays back as a session the client cannot read.
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

Six principles do most of the work. Read them before you write a document, and apply them when you
edit one:

1. **Introduce a term where you first use it**, in the same sentence, or link to the page that
   defines it. A reader should never meet a word the docs have not explained.
2. **State the point first, then qualify it.** Opening with the qualifier makes the reader decode
   the sentence backwards.
3. **Give every sentence a subject and a verb.** "Two version numbers, one verdict, one remedy"
   reads as knowing rather than clear. Say what the thing is.
4. **A how-to is steps that work.** Put the reasons somewhere else. A reader working through
   one wants commands that run.
5. **Describe what the software does, not how it came to do it.** Leave out what the project used
   to do, what was tried and dropped, and numbers from a run somebody did once.
6. **Do not explain a design by contrast with a worse one.** Say what it is and what you get,
   rather than asking the reader to picture a design nobody proposed.

The mechanical rules are enforced rather than restated here.
[`cs-lint`](https://github.com/codesweep-ai/lint) carries them, and `make check` runs it over this
repository. To read what a rule wants and the guidance behind it:

```bash
cs-lint prose --explain
```

That listing is the authority. Where this section and the linter disagree, the linter is right.
Turning a check off is a waiver: write it under `allow` in [`.cs-lint.yaml`](.cs-lint.yaml) with the
reason, which is printed with the finding.

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
