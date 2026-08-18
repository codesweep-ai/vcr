# Contributing

These rules apply to **humans and coding agents alike**. If you are an agent working in this repo,
read this file before you change anything and follow it.

Bug reports and pull requests are welcome. For a security issue, use GitHub's private
vulnerability reporting on this repository's Security tab, rather than opening a public issue.

## Before you push

```bash
make check        # gofmt, vet, unit tests, the race detector and every linter
make test-smoke   # three real agents replaying committed cassettes, about 5s
```

`make check` shells out to two tools that do not come with Go, and the ledger below needs a third.
Install all three once, pinning `golangci-lint` to the version CI runs:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
go install golang.org/x/tools/cmd/deadcode@latest
go install github.com/codesweep-ai/ledger/cmd/cs-ledger@latest
```

This repo keeps a **ledger** of open issues in `ledger/`. Read
[`ledger/AGENTS.md`](ledger/AGENTS.md) before you start work, and follow it as you go. A commit
that touches `ledger/` needs `cs-ledger render && cs-ledger check` to pass first, and `make ledger`
runs the check half.

## The one thing this repo will not trade away

**Replay must not be able to spend money.** It never falls through to upstream, however it is
configured. That is why the command that built the server sets `ReachesUpstream`, rather than each
call site branching on it. The test asserts it with an upstream that fails if it is dialled at
all. A change that lets a miss reach a provider "just this once" turns a $0 CI bill into a
surprise invoice, and will be rejected however convenient it is.

cs-vcr also does not authenticate callers, hold credentials, swap keys or redact anything. A patch
adding any of those is adding a second product. Request headers are never recorded, which is what
makes "no redaction" safe rather than reckless, so keep it that way.

## Tests are part of the change

Every behavior change ships with test coverage. A change with no test is only acceptable when the
behavior genuinely cannot be observed in a test — say so in the PR.

- `make test` is the tier a change usually needs. Everything cs-vcr does is observable from a Go
  test: the cobra tree driven with a fake environment, the proxy driven against a local upstream, a
  cassette in a temp dir. That is where the costly-if-silently-wrong things live, such as the
  record/replay round trip and SSE framing.
- `make test-integration` replays the committed cassettes with the real Claude Code, Codex and OpenCode.
  It needs no credentials and contacts no provider. Run it after touching normalization, the SSE
  handling or the cassette format — those are the things a hand-written fixture cannot check. When a
  change alters what a real session sends, re-record with `make fixtures` (see
  [INSTALL.md](INSTALL.md#4-run-the-live-agent-suite)) and commit the cassettes with the code.
- `make test-smoke` is a named subset of that: one scenario per agent, covering all three surfaces,
  in about five seconds. It is the one to run before every push. CI runs the full matrix, which is
  cheap enough not to need a subset — this profile is for you, not for it.
- **Every gate fires with both a positive and a negative test.** The negative half is usually the
  one that matters: that an unknown replay refuses *and did not dial*, or that a miss *fails the
  session* rather than passing quietly.
- Test the contract, not the implementation: the exit code, the bytes that reached the client, the
  header that arrived upstream. Say *why* the case matters in a comment when it isn't obvious.

## Cassettes

Cassettes are committed and reviewed, which is what the directory-per-cassette format is for. When
a change alters recorded traffic, the diff has to be legible in the PR. If it is not, fix the
format rather than asking the reviewer to cope.

`cassette verify` gates merges. A change to the normalization ruleset means bumping
`normalize.version` and re-recording, in the same PR.

## Commits

Make one commit per idea. If a change will not fit that shape, it is doing more than one thing, so
split it.

**Subject** — always. Under 60 characters, imperative, no trailing period, completing *"If applied,
this commit will …"*. Say what the change does.

**Body** — add one only when the subject leaves a real question. Use bullets, one line each, under
60 characters, describing the design: the shape the change takes, or the constraint that ruled out
the obvious alternative. Do not describe the diff, or how you arrived at it. Write as many bullets
as there are points and no more. Most commits need none, one is common, three is the rare maximum.

Leave out why the work was scheduled, how it was tested, and what prompted it: rationale belongs in
a comment or `SPEC.md`, evidence in the PR.

```
Fix the port parse in the sidecar redirect rule
```

```
Replay SSE per event, not as one write

- A client assembling deltas sees framing, not bytes.
```

Keep the `Co-Authored-By:` trailer when an agent wrote the change. Drop any trailer linking to the
agent's session or transcript — private to whoever ran it, dead to everyone else.

## Docs

Behavior a user can see belongs in the docs next to it, updated in the same commit as the code.
`README.md` is the tour, `INSTALL.md` says how to get the binary, `MANUAL.md` is the command
reference, and `SPEC.md` says what cs-vcr guarantees and how it is built. A change to a **MUST** in `SPEC.md` changes the contract, so say so
in the PR.

## Writing

The docs are for someone who has not read the code. `scripts/lint-docs.py` checks the mechanical
part of this, and `make docs` runs it. Its knobs live beside it in `scripts/lint-docs.config.py`,
which carries this project's glossary. The linter itself is vendored and stays byte-identical
across projects, so a fix to a check belongs in the shared copy.
`scripts/lint-oss.py` is its sibling, and `make oss` runs it. It checks what this repository has to
satisfy as a published project, and `--explain` lists every rule it applies. Its knobs live beside
it in `scripts/lint-oss.config.py`.

- **Introduce a term where you first use it**, in the same sentence, or link to the page that
  defines it. A reader should never meet a word the docs have not explained.
- **Give every sentence a subject and a verb.** "Two version numbers, one verdict, one remedy" reads
  as knowing rather than clear. Say what the thing is.
- **State the point first, then qualify it.** Opening with the qualifier makes the reader decode
  the sentence backwards.
- **Keep sentences under 30 words**, and to one idea each.
- **Use at most one em-dash per paragraph.** Where a second one appears, a full stop usually works
  better.
- **Address the reader as "you"**, and use the imperative for steps.
- **Keep the evidence out of the instructions.** A war story explains a decision; put it in an
  explanation section, not in the middle of a task.
- **Make every example runnable as written.** If a step invokes a script, show the script first. A
  reader should never meet a file they were not given.
- **Do not comment on your own writing.** "It is worth stating plainly", "put simply", "the point
  is" — delete the frame and keep the sentence.
- **Do not explain a design by contrast with a worse one.** "A directory, so a change reads as a
  diff rather than as one unreadable line" asks the reader to picture a format nobody proposed. Say
  what it is and what you get.
- **Leave out what does not matter.** If a setting is ignored, do not name it. Every fact you print
  is one the reader has to decide whether to act on.
- **A walkthrough is steps that work.** Put the reasons somewhere else. A reader following a
  walkthrough wants a config that runs, not an account of which API has a `/v1` and why.
- **An ordered procedure is a numbered list, not a sentence.** A requirement that says "MUST, in
  order: mount the filesystems; create the user; start the daemon" is unreadable, and trips the
  length check for a reason. Break it into steps the reader can follow one at a time.
- **Describe what the software does, not how it came to do it.** Leave out what the project used to
  do, what was tried and dropped, and numbers from a run someone did once. A rule's reason belongs
  beside the rule. The investigation that found it belongs in the PR.
- **Do not make the reader hold two halves of a sentence apart.** "What a shell printed may differ;
  what the model was asked may not" is a puzzle. Name the subject in each clause.

What not to change: the voice is concrete, opinionated and free of marketing padding. These rules
are about mechanics. Where one of them fights the voice, the voice wins.
