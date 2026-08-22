# The cs-vcr specification

cs-vcr sits between an AI coding agent and its LLM provider. It records the HTTP traffic into a
**cassette**, and later replays that cassette so a run exercises the whole agent loop without
calling a provider.

This document specifies the tool completely enough to work on it, extend it, or rebuild it on
another stack. Requirements use RFC 2119 keywords in bold: **MUST** is an obligation any
implementation has to meet, **SHOULD** is a strong preference, and **MAY** is a genuine option.
Prose in *italics* explains a decision and obliges nothing.

## 1. Purpose

An agent-driven build costs money to run and gives a different answer each time. Both properties
make it unusable as a test. A recorded run costs nothing to repeat and gives the same answer every
time.

### 1.1 Goals

1. A recorded session **MUST** replay with no provider reachable and no credential configured.
2. A cassette **MUST** be reviewable as a text diff in a pull request.
3. Pointing an agent at cs-vcr, at any cassette, **MUST** require no change to it beyond a base URL.
   An agent that contacts hosts of its own also takes a proxy setting, naming that same address.
4. An agent's own credential **MUST** reach the provider unchanged.

### 1.2 Non-goals

cs-vcr does not authenticate callers, hold or swap credentials, enforce budgets or quotas, or model
a provider's semantics beyond routing. Nothing in the request path redacts anything: headers cross
untouched and are never recorded, so there is no credential in a cassette to redact and no redactor
to keep correct.

`cassette scrub` is the one place a value is removed, and it is not in the request path. It is a
command you run over a finished cassette, before committing it, over what the SESSION put in a body.

It does not make the agent's environment deterministic. A shell command that prints something
different on the replay run still prints something different, and no rule should reconcile the two.

## 2. Vocabulary

| Term | Meaning |
|---|---|
| **cassette** | A directory holding one recorded session, committed to the repo it belongs to. |
| **step** | One request and the response it received. A cassette is an ordered list of steps. |
| **script** | The steps of a cassette, in the order the session made them. |
| **client** | The agent making a request, as cs-vcr sees it: an HTTP client and a base URL. |
| **surface** | The API shape a request belongs to, such as `anthropic.messages`. |
| **canonical request** | The request body reduced to a comparable form: keys sorted, rules applied. |
| **alignment** | The comparison of a live request against the recorded one, field by field. |
| **volatile path** | A JSON path where a difference is the world's answer, not the agent's decision. |
| **drift** | A tolerated difference at a volatile path, reported rather than absorbed. |

## 3. Interfaces

cs-vcr exposes three interfaces.

| Interface | Purpose |
|---|---|
| **Proxied port** (default `127.0.0.1:8080`) | Agent traffic. Everything on it is proxied, recorded or replayed. |
| **Admin port** (default `127.0.0.1:8081`) | `GET /healthz` only, so a script can wait for readiness. |
| **Command line** | `record`, `replay`, `cassette`, `calibrate`, `config`, `manual`, `version`. |

The admin endpoint **MUST NOT** be served on the proxied port. An unrecognized path on the proxied
port is forwarded upstream, so `/healthz` there would become a request to a provider.

### 3.1 Commands

`record` and `replay` are two commands rather than one command with a mode flag. Which one you run
decides whether the session can spend money.

| Command | Effect |
|---|---|
| `cs-vcr record` | Proxies to providers, appending each interaction as a step. |
| `cs-vcr replay` | Serves only from the cassette. Built with nowhere to send a request. |
| `cs-vcr cassette ls [NAME]` | Lists cassettes, or the steps in one. |
| `cs-vcr cassette show NAME STEP` | Prints one step's metadata, request and response. |
| `cs-vcr cassette verify [NAME...]` | Checks entries against the current ruleset. Exits non-zero when stale. |
| `cs-vcr cassette scrub [NAME...]` | Reports credentials and personal data. `--force` removes them. |
| `cs-vcr cassette prune NAME` | Reports unreferenced body files. `--force` deletes them. |
| `cs-vcr calibrate NAME MISSDIR` | Proposes `volatile` paths from a failed replay. |
| `cs-vcr config [AGENT]` | Prints the resolved configuration, or how to point an agent at a cassette. |
| `cs-vcr manual` | Prints `MANUAL.md`, embedded in the binary at build time. |
| `cs-vcr version` | Prints the version, platform and Go version. |

`record` and `replay` accept `--listen`, `--admin` and `--cassettes`. `replay` also accepts
`--dump-misses DIR`. Neither names a cassette: a request does that itself.

## 4. The request path

```
   agent
     │  POST /c/refactor-auth/v1/messages   authorization: Bearer …
     ▼
  ┌──────────────────────────────────────────────────────────────┐
  │ identify        /c/<name> -> cassette, prefix stripped       │
  │ classify        path, then the cassette's pinned provider    │
  │ normalize       canonical form of method, target and body    │
  │                                                              │
  │ replay          next step of the script, verified by         │
  │                 alignment ─▶ serve, or fail the run          │
  │ record          forward, then append a step                  │
  │ forward         headers cross unchanged                      │
  └──────────────────────────────────────────────────────────────┘
     │  POST /v1/messages   authorization: Bearer …
     ▼
   provider
```

**R1.** The cassette **MUST** be established first, and the prefix **MUST** be stripped before
anything else reads the path. *Otherwise every request classifies as an unrecognized surface, and
the provider receives cs-vcr's own addressing.*

**R1a.** A `/c/<name>` prefix **MUST** name the cassette directly, with nothing declaring it. A
request carrying no prefix **MUST** be refused. *A scenario that has to be declared in a second file
before it can run is a scenario that stops a build until two files agree about it. A default to
absorb an unprefixed request into would undo that: a base URL missing its prefix would look like it
worked while its traffic landed in another scenario's recording. The prefix reaches everything a
client sends, because it is part of the base URL every request is built from. Measured on Claude
Code's `HEAD /api/hello` preconnect, which sets no headers at all and still carries it.*

**R1b.** A cassette name **MUST** be one path segment of letters, digits, dot, dash or underscore,
beginning with a letter or digit. A name that is not **MUST** be refused. *The name arrives in a URL
and becomes a directory, so `/c/../../etc` would otherwise read outside the store.*

**R1c.** A cassette named by a prefix **MUST** be opened when its first request arrives. `record`
**MUST** create one that is absent; `replay` **MUST** refuse it. *Replay creating one answers every
request with a miss, which reads as an agent that diverged rather than as a base URL with a typo in
it.*

**R2.** Request headers **MUST** cross unchanged, including the credential.

**R3.** A recording session **MUST NOT** serve any request from the cassette. Every call the session
makes, including one it makes twice, **MUST** reach the provider. *A cassette is the record of a
session, so serving one call from it would leave a script with a step missing from the middle.*

**R4.** A replay session **MUST NOT** open a connection to a provider, whatever the configuration
says.

**R4a.** A replay session **MUST NOT** read provider configuration at all, and **MUST** start and
serve with none configured. *Goal 1 asks a recorded session to replay with no provider reachable and
no credential configured. Resolving the upstream before the replay branch met the first and not the
second. A request would then fail for want of a provider it could never have called, and only the
defaults naming two of them hid it. Validating that configuration at startup was the same defect one
step earlier, and quieter: a `replay` session refusing to start over a `base_url` it would never
have dialled.*

## 5. Matching

Replay proceeds in two stages. It **selects** a step by position, then **verifies** that selection
by aligning the request that arrived against the one recorded there.

### 5.1 Selection

```
1. scan forward over unserved steps from the step the session is at, reaching at most
   `lookahead` steps past the furthest step already served
2. serve the first that aligns: same method, same path, and a body that aligns
3. if none does, serve again the most recently served step the request aligns with
4. answer a bookkeeping call from any recorded bookkeeping call, unless `auxiliary_turns` is off
5. otherwise report a miss
```

**R5.** Selection **MUST** compare the method and path as well as the body. *Every agent's startup
probes are bodiless, so two of them would otherwise align with each other trivially.*

**R6.** cs-vcr **MUST** serve an already-served step again when nothing new aligns and that step
does, taking the most recently served of them. *This is a client retrying, or a probe it issues more
often on one run than on another. Codex asks for the model list at startup and does not always ask
the same number of times. The extra ask can arrive after the session has moved past the step that
answers it.*

**R7.** A step served out of order **MUST** be served, and **MUST** be reported and counted.

**R8.** A miss **MUST** report the step it could have been, naming the recorded request and the
paths that disagreed.

*Why a window rather than strict order: clients pipeline. Codex issues two `GET /models` at startup,
and Claude Code runs title generation alongside its main loop. A larger window cannot serve a wrong
step, because alignment is exact, so a step that aligns is this request. The window bounds the
search, not the similarity: too small costs a loud failure, never a quietly wrong answer.*

*Why it is measured from the furthest step served rather than from the session's own position: a
client may record a request it does not always make. Claude Code's title generation is one, running
beside the main loop. Anchored at the cursor, one such step pins the window for the rest of the
session. Every later request is then measured from a step that will never be served, and a 45-step
recording replayed 2 of them.*

### 5.2 Alignment

Alignment walks the recorded request and the live one in parallel and reaches one of three verdicts.
There are no thresholds anywhere.

| Verdict | Condition | Result |
|---|---|---|
| mismatch | Shapes differ: a key on one side, arrays of different lengths, another type. | Fail, naming the path. |
| mismatch | A value differs at a path not declared volatile. | Fail, naming the path and both values. |
| tolerated | A value differs under a declared volatile path. | Serve, and report it. |

**R9.** A difference under a volatile path **MUST** be tolerated, and **MUST** be reported and
counted as drift.

**R10.** A volatile declaration **MUST** cover the shape beneath the path as well as its values. *A
per-run block gains and loses keys between runs, so a check that only excused leaves would report
the client's own noise as a divergence.*

**R11.** Nothing **MUST** be treated as volatile that is not declared.

**R12.** A length difference in an array **MUST** be reported at the array, listing what each side
holds, and **MUST NOT** be reported once per element. *Pairing items across a length change means
guessing which one went, and a guess names the wrong item with total confidence.*

The line this draws is **rough about the world, exact about the agent**. What the model decided must
be identical: the model itself, its tools, the instructions, and each tool call's name, arguments
and id. What the world answered back may differ, because cs-vcr replays the model and never claimed
to reproduce a shell.

The shipped defaults name a tool result on each surface, and none of them names a tool call:

```yaml
normalize:
  volatile:
    - input[].output                  # OpenAI responses: what a tool call answered
    - messages[].content[].content    # Anthropic messages: tool_result
    - messages[].content[].is_error   # Anthropic messages: whether that tool failed
    - messages[role=tool].content     # OpenAI chat: a tool result is a message of its own
```

### 5.3 Normalization

Alignment compares canonical requests: the body with keys sorted, insignificant whitespace removed,
and the configured rules applied. `<`, `>` and `&` are left unescaped, because a prompt is full of
them and an escaped form is neither readable in review nor addressable by a rule.

**R13.** Each `strip_fields` path **MUST** be removed from the body before comparing.

**R13a.** Each `drop` marker **MUST** remove the whole list item that carries it, on both sides,
before comparing. *Codex assembles its instruction preamble from what the installation has, and
sends each part as a content item of its own. An item present in one run and absent in the next
changes the list's length, and a list whose length differs aligns with nothing.*

**R14.** Each `strip_query` parameter **MUST** be removed from the request target. The query
**MUST NOT** be dropped as a class, because a parameter can select provider behaviour.

**R15.** Each `replace` rule **MUST** be applied in order. Replacements are one-way, and **MUST NOT**
be applied to responses.

**R16.** Each `capture` match **MUST** be blanked for comparison, and this run's value **MUST** be
restored into the response on the way out.

**R17.** Distinct values matching one capture pattern **MUST** receive distinct placeholders,
numbered by order of first appearance. *An orchestrator holds its own dispatch id and the one it
minted for a member. Collapsing both into one placeholder restores them to the same value, and the
replayed orchestrator then polls a session it never prompted.*

*Why capture rather than replace: the agent acts on some of these values. Blanking a scratchpad path
one way would make the request match, then hand the replayed agent the recording run's path, which
does not exist on this machine.*

## 6. Routing

A request is routed by path first, then by the pin its cassette carries. Routing names an upstream,
so it is a question only a session that forwards asks. Replay reads the surface off the path, which
is all it reports by, and stops there (R4a).

| Path | Surface | Provider |
|---|---|---|
| `/v1/messages` or `/messages` | `anthropic.messages` | anthropic |
| `/v1/responses` or `/responses` | `openai.responses` | openai |
| `/v1/chat/completions` or `/chat/completions` | `openai.chat` | openai |
| anything else | `unknown` | see below |

**R18.** A surface **MUST** be recognized with or without the `/v1` prefix, and no other version
prefix **MUST** be treated as `/v1`. *The version sits in the base URL a client is pointed at. Codex
signed in with ChatGPT talks to a backend whose endpoint is `/responses`, with no version at all.*

**R19.** A path cs-vcr does not model **MUST** still be proxied, recorded and replayed. *A request
that is proxied but not recorded is one replay can never serve.*

**R20.** When a cassette is pinned to a `provider`, every request on its prefix **MUST** go there,
whatever the path.

**R21.** Otherwise an unrecognized path **MUST** be routed by the Anthropic-specific request headers
when present, and by `default_provider` when not.

*Guessing from the rest of the request does not work, and the failure is quiet. A Pro/Max
subscription login sends `Authorization: Bearer` exactly like an OpenAI client, and Claude Code's
`HEAD /api/hello` startup probe carries no identifying header at all.*

### 6.1 Tunnelling

A base URL aims an agent's model calls at cs-vcr. It does not aim the rest. Claude Code checks its
OAuth session against `api.anthropic.com`. Codex reaches `chatgpt.com` for its subscription
transport and `ab.chatgpt.com` for experiment assignment. Each happens whatever base URL the client
was given.

Those answers change the prompt. A real login makes them succeed. A fabricated one makes them
return 401, and the request the agent sends next is not the one the cassette holds.

So cs-vcr answers `CONNECT` on the listener it already serves, and an agent may point `HTTP_PROXY`
at the same address as its base URL.

**R30.** A `CONNECT` to a host on the refusal list **MUST** be refused, in a recording session as
well as a replaying one. *Blocked in both halves, the two runs ask the same question. Blocked in
one, a recording carries a block of prompt that a replay cannot rebuild.*

**R31.** Every other host **MUST** be tunnelled. *An agent's tools share its environment. A blanket
refusal takes away `git`, `curl` and every package manager the agent might shell out to, and
nothing they return changes the prompt.*

**R32.** A replaying session **MUST** also refuse the providers it was configured with. *It says of
itself that it contacts none, and a tunnel is the one way a client could reach one through cs-vcr
anyway.*

**R33.** A tunnel **MUST NOT** count as a request. *Requests are what a session can record, and a
recording asserts that it recorded every one of them. A tunnel records nothing.*

No certificate is involved. A `CONNECT` proxy pipes bytes, and TLS stays end to end between the
client and the host it dialled. The hostname this decides on is the one in the `CONNECT` line.

## 7. Recording

**R22.** cs-vcr **MUST** record the response bytes the client received, captured as they pass
through rather than re-encoded.

**R23.** A response **MUST** be recorded as streaming when its content type is `text/event-stream`,
or when no content type is set and the body opens with `event:` or `data:`. *An upstream is not
obliged to label a stream, and one that matters does not.*

**R24.** A stream **MUST** be replayed one event at a time, flushed after each, with the recorded
content type verbatim. *A client picks its parser from the content type, and a client that assembles
deltas incrementally behaves differently against a single write.*

**R25.** Response bodies **MUST** be stored uncompressed. cs-vcr **MUST** drop the client's
`Accept-Encoding` and negotiate its own with the provider. *A gzipped cassette cannot be read in a
diff, which is the whole reason for the format.*

**R26.** A streamed run of fragments the client reassembles **MUST** be joined into one event before
storing: a tool call's arguments, and a model's reasoning. The visible answer **MUST NOT** be.
*A model splits fragments at arbitrary character boundaries, so a value inside one is not contiguous
and nothing can substitute it, measured on three surfaces, each with its own spelling. Reasoning is
included because a client carries it into the next request as conversation state. The answer is
excluded because it is rendered to a person as it arrives, and those boundaries are what a replayed
session reproduces.*

**R27.** A response that ended early **MUST** still be recorded, and the interruption **MUST** be
logged. *A client hangs up once it has read the last event it wants, and the bytes it received are
the interaction. The case that is not benign looks identical from here, so the run says so.*

The copy a recording keeps **MUST** be bounded. A response that outgrows the bound **MUST** reach
the client in full, **MUST** be recorded as far as the bound, and **MUST** be reported. *A stream
runs for as long as the model keeps talking, and nothing in HTTP obliges it to stop. An unbounded
copy ends the session in an out-of-memory kill that reports nothing and loses every step already
captured.*

**R28.** A transient failure (408, 409, 429, or any 5xx) **MUST** be recorded as a step like any
other, and **SHOULD** be flagged. *A cassette carrying a rate limit replays that rate limit, and the
client retries it exactly as it did.*

**R29.** Request headers **MUST NOT** be recorded. *A cassette needs the body to match on and the
response to replay, and neither needs an `Authorization` header. There is then no credential to
redact, and no redactor to keep correct as header names change.*

**R30.** Response headers **MUST NOT** be recorded or replayed, with two exceptions: the content
type, and a `Retry-After` in delay-seconds form on a status a client retries. *The rest carry a
recording run's session cookie and a stale timestamp, and replaying those causes misses rather than
preventing them. `Retry-After` is what a client reads to decide how long to wait, so a recorded rate
limit that drops it replays as a run that backed off differently. The date form of the header is a
moment in the recording run, and a replayed deadline has always already passed.*

## 8. Errors, accounting and diagnostics

Every error cs-vcr generates is a JSON object with `error.type`, `error.message` and
`error.source: "cs-vcr"`.

| `error.type` | Status | Condition |
|---|---|---|
| `no_cassette` | 404 | The base URL does not end in `/c/<name>`, so the request names no cassette. |
| `bad_cassette_name` | 400 | The prefix names something that is not a cassette name. |
| `unreadable_body` | 400 | The request body could not be read. |
| `unknown_cassette` | 404 | Replay was asked for a cassette the store does not hold. |
| `cassette_unusable` | 400 | The cassette a request named will not open, usually a version that moved. |
| `cassette_miss` | 400 | Replay has no step for this request. |
| `cassette_corrupt` | 400 | The index references a response file that is absent. |
| `no_provider` | 502 | No upstream is configured for the routed provider. |
| `bad_base_url` | 502 | A provider's `base_url` will not parse. |
| `upstream_error` | 502 | The upstream request failed. |

**R31.** A refusal no retry can fix **MUST** answer 400: a miss, a cassette that will not open, an
index entry whose response file is absent. It **MUST NOT** answer a status clients retry, and
**MUST NOT** answer 404 on a model endpoint. *Stainless-generated SDKs retry a 5xx, which turns two
misses into sixteen requests. A 404 on `/v1/messages` is how the API reports an unknown model. One
miss therefore reached an operator as "that model may not exist or you may not have access to it".
The same trap caught a cassette from an older ruleset, which answered 500 and sent the client into a
backoff loop against a condition that would never improve. The failure reached its reader as a
ten-minute hang rather than as one line naming both versions.*

**R32.** A request that names no cassette, or names one that cannot be used, **MUST** be refused.
*Otherwise a mistyped base URL looks like it worked while its traffic lands in another scenario's
recording, or in nothing at all.*

**R33.** A replay session that ends with one or more misses **MUST** exit 4.

### 8.1 Session summary

Both commands print a summary on exit. It is the artifact a CI log shows.

| Line | Meaning |
|---|---|
| `requests` | Requests received. |
| `replayed` / `recorded` | Steps served from the cassette, and appended to it. |
| `upstream calls` | Requests that reached a provider. Always 0 under `replay`. |
| `misses` | Requests with no recording. Fails a replay session. |
| `unknown cassette` | Requests that named no cassette, or named one that could not be used. |
| `rejected` | Requests answered with an error. |
| `abandoned` | Printed when the session exited with requests still in flight. |
| `drifted observations` | Printed when a difference was tolerated at a volatile path. |
| `out of recorded order` | Printed when a step was served out of sequence. |
| `tunnelled` / `tunnel refused` | Printed when the session was used as a proxy. See section 6.1. |
| `surface …` / `cassette …` | Requests per surface, and per cassette the session touched. |

### 8.2 Calibrate

`cs-vcr calibrate NAME MISSDIR` pairs each request that `replay --dump-misses` wrote with the step
it was compared against, aligns the two, and prints the paths that differed as configuration.

**R34.** `calibrate` **MUST** print configuration that parses, and **MUST NOT** apply it. *The
judgement it cannot make is the one that matters: whether a path that differed is the world
answering differently, or the agent being asked something else.*

**R35.** `calibrate` **MUST NOT** propose a rule for a shape difference. *An item added or removed
means the request is built differently, and declaring the enclosing list volatile would blank it.*

**R36.** `calibrate` **MUST** report requests it could not pair with a step. *Pairing one with a step
by guesswork is how a proposal names the wrong path with total confidence.*

**R37.** `--dump-misses` **MUST** name each file after the step it was compared against, and
**MUST** be off unless asked for. *Replay reads a cassette and should not dirty the checkout it was
given.*

## 9. The cassette format

A cassette is a directory. Each request and response is its own file, so a one-word prompt change
shows up in review as a one-word diff.

```
cassettes/refactor-auth/
  cassette.yaml       versions and provenance
  index.jsonl         one line per step, in the order they happened
  req/0001.json       the canonical request, pretty-printed
  resp/0001.json      a non-streaming response body
  resp/0003.sse       a streamed response, its events in order
```

### 9.1 cassette.yaml

```yaml
format_version: 3
created: 2026-08-12T18:04:11Z
proxy_version: 0.1.0
normalize_version: 11
```

`format_version` covers everything the build decides: the layout, and the canonical form a request
is reduced to. `normalize_version` is the ruleset's claim about which requests are equivalent. The
ruleset is configuration, so it carries a number of its own.

**R38.** `record` and `replay` **MUST** refuse a cassette whose `format_version` or
`normalize_version` differs from this build's, and **MUST** name which one moved. *Otherwise every
entry parses, nothing errors, and every request misses, which reads as an agent that changed its
mind rather than as a version that moved.*

**R39.** `cassette ls`, `show` and `verify` **MUST** still read such a cassette. *`verify` exists to
report exactly this, and a listing that refuses to list takes the diagnosis away with the failure.*

### 9.2 index.jsonl

Each line is one JSON object, and the lines are in session order.

```json
{"seq":3,"hash":"8f2a3c1d9e0b…","method":"POST","path":"/v1/messages?beta=true",
 "provider":"anthropic","surface":"anthropic.messages","model":"claude-sonnet-5",
 "status":200,"streaming":true,"content_type":"text/event-stream",
 "recorded_at":"2026-08-12T18:04:11Z","latency_ms":4120}
```

| Field | Meaning |
|---|---|
| `seq` | Position in the script, from 1. Names the body files. |
| `hash` | Identifies the request in logs and in `cassette show`, which shorten it as above. Selects nothing. |
| `method`, `path` | Checked during selection. `path` is the target after `strip_query`. |
| `provider`, `surface`, `model` | Metadata for `cassette ls`. |
| `status`, `streaming`, `content_type` | Replayed to the client. |
| `retry_after` | Replayed to the client. Present only on a status a client retries. |
| `recorded_at`, `latency_ms` | Provenance. |

**R40.** Bodies **MUST** be addressed by position, not by content hash. *Two steps of one session
can be the same request with different answers, and content addressing gives those one file, so the
second answer overwrites the first.*

**R41.** The index **MUST** be appended to and never rewritten. *An interrupted session then leaves
every completed step intact, with at worst one truncated final line.*

**R42.** A cassette **SHOULD** hold one session. *Recording two into one leaves a script holding
both, and replaying either has to reach past the other.*

### 9.3 Streamed responses

Stored as an ordered list of SSE events:

```
event: message_start
data: {"type":"message_start","message":{"id":"msg_01…"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Ref"}}
```

Keeping the events separate is what lets a client behave on replay as it did on the recording.
Replay reproduces those event boundaries, as fast as the client will take them. It does not
reproduce the original TCP chunk boundaries or the timing between events, and no client can tell: no
HTTP client API exposes where a TCP write landed.

### 9.4 Scrubbing, before a cassette is committed

Request headers are never recorded, so the credential an agent authenticated with is not in a
cassette. What is in one is whatever the session put in a **body**. A key quoted in a
prompt, a token a tool call carried, the address the agent was told its user has: all of those are
prompt text. A cassette is committed, so there is a pass between recording one and publishing it,
and `cassette scrub` is that pass.

**R48.** `scrub` **MUST** change nothing unless asked, and **MUST** exit non-zero while a cassette
still holds a value it recognizes. *It is a gate, so its answer has to be an exit code.*

**R49.** A report **MUST** name each finding by kind and file, and **MUST NOT** print the value. *A
report is read in a CI log and pasted into an issue, and one that quotes the secret has published it
a second time.*

**R50.** A secret the caller names **MUST** be read from the environment rather than from the
command line, and one that could not be looked for **MUST** be reported. *Every process on a machine
can read another's arguments, and an unset variable must not print as "clean".*

Removing a value from a **request** changes what replay matches on, and that is the point. Such a
value was going to make the cassette replay for nobody but the person who recorded it. The remedy is
a `normalize` rule, which blanks it on both sides, so a scrub that breaks a replay has found a rule
that is missing.

## 10. Configuration

Every setting is optional, and so is the file: cs-vcr runs with none at all. Flags beat the
environment, which beats the file.

```yaml
listen: 127.0.0.1:8080
admin: 127.0.0.1:8081
cassettes: ./cassettes
lookahead: 8
auxiliary_turns: true

providers:
  anthropic: {base_url: https://api.anthropic.com}
  openai: {base_url: https://api.openai.com}
default_provider: anthropic

cassette_provider:
  refactor-auth: anthropic

normalize:
  version: 11
  volatile: ["input[].output", "messages[].content[].content"]
  strip_fields: [client_metadata, prompt_cache_key]
  strip_query: [client_version]
  drop: ["<plugins_instructions>"]
  replace:
    - {pattern: "(Today's date is )\\d{4}-\\d{2}-\\d{2}", with: "${1}<DATE>"}
  capture:
    - {pattern: "dispatch-[0-9]{10,}", as: "<DISPATCH>"}
  root: /abs/path
```

**R43.** An unknown key **MUST** be an error. *The failure this file most invites is "I set it and it
was ignored".*

**R43a.** A config file named outright, by `--config` or `CS_VCR_CONFIG`, **MUST** exist. *The
default location holding nothing is the ordinary case, and not an error. Naming a file that is not
there is a typo. A session that shrugged at it would run on settings its caller did not write, which
is R43's failure again, one step before the file is opened. `CS_VCR_HOME` names a place to look
rather than a file, so it is not this case.*

**R44.** An invalid `replace` or `capture` pattern **MUST** fail at startup, not per request.

**R44a.** A provider's `base_url` **MUST** be a URL with a scheme and a host, checked at startup with
the parser the request path uses. *Otherwise what passes validation is not what forwards.*

**R45.** A `cassette_provider` key **MUST** be a valid cassette name. *It names a cassette, and a
key no prefix can ever produce is a pin that silently never applies.*

**R46.** Every provider a request could be routed to **MUST** name a configured provider, checked at
startup. That is `default_provider` and each `cassette_provider` value. *A typo
otherwise surfaces as a 502 partway through a recording session, and for `default_provider` that is
the first request: the startup probes a client opens with are exactly the paths cs-vcr does not
model.*

**R47.** The configuration **MUST NOT** contain a credential.

`volatile`, `strip_fields` and `strip_query` take JSON paths, where `[]` descends into every element
of an array and a path covers everything beneath it. A volatile path may instead name an array
element by the role it carries, as in `messages[role=tool]`. That reaches a tool result on a surface
which puts one beside the prompt in the same list. `drop` is not a path: it names the opening of a
block, and removes whichever list item carries it.

| Variable | Effect |
|---|---|
| `CS_VCR_CONFIG` | Config file path. The file has to exist. |
| `CS_VCR_HOME` | Root the config file is looked for under, when `CS_VCR_CONFIG` is unset. |
| `CS_VCR_CASSETTES` | Cassette store directory. |
| `VCR_LISTEN` / `VCR_ADMIN` | Listen addresses. |
| `VCR_ROOT` | Checkout root, where it is not the working directory. |

## 11. Implementation

### 11.1 Packages

| Package | Role |
|---|---|
| `cmd/cs-vcr` | Entry point. Maps errors to exit codes. |
| `internal/cli` | The command tree, server lifecycle, cassette and calibrate commands. |
| `internal/config` | Configuration model, defaults, normalization rules. |
| `internal/proxy` | The HTTP handler, routing, the response tap. |
| `internal/cassette` | The on-disk format, canonicalization, alignment, the store. |
| `internal/paths` | XDG paths. |
| `vcr` (root) | The embedded `MANUAL.md` that `cs-vcr manual` prints. |

Dependencies run `cli → proxy → cassette` and `cli → proxy → config`. The cassette package does not
import config: config satisfies a `Ruleset` interface the cassette package defines, so the format
stays specified independently of the settings that feed it.

### 11.2 Key types

| Type | Role |
|---|---|
| `proxy.Server` | The `http.Handler`. Holds the config, the stores and the counters. |
| `proxy.tap` | A `ResponseWriter` that forwards to the client and keeps a copy. |
| `cassette.Store` | The script, a cursor into it, and the selection logic. |
| `cassette.Alignment` | The verdict of comparing two canonical requests. |
| `cassette.Key` | The canonical request, its hash, and what the captures matched. |

### 11.3 The safety property

`ReachesUpstream()` is the one asserted bit, and the command that built the server sets it. `replay`
builds an offline server.

It **MUST** be a constructor argument rather than an optional method. A caller can silently omit an
optional method that grants safety, and the result still compiles and still reports itself as
offline while dialling. One place **MUST** read it, rather than each call site branching on it,
because a rule enforced once cannot be forgotten twice.

The same bit decides which configuration a session reads. `record` validates every provider it could
route to; `replay` validates the ruleset and nothing else, because R4a is a claim about startup as
well as about the request path.

### 11.4 Lifecycle and concurrency

- Both listeners **MUST** be opened before either serves, so a port clash fails startup rather than
  leaving the proxy up with its admin endpoint missing.
- A stopping session **MUST** drain the proxied listener, because a request in flight is a step not
  yet written. The wait is bounded, and a second signal ends it. What the wait did not cover
  **MUST** be counted as `abandoned`.
- The recording **MUST** be written from a deferred call. *When a client hangs up mid-response the
  reverse proxy abandons the request with `panic(http.ErrAbortHandler)`, which `net/http` recovers
  silently, so anything after `ServeHTTP` would be skipped.*
- The store guards the script, cursor and body cache with one mutex. The server guards its counters
  with another.

### 11.5 Testing

There are two tiers, and they prove different things.

**The unit suite** drives everything cs-vcr does from a Go test: the command tree with a fake
environment, the proxy against a local upstream, cassettes in temporary directories. `make check`
runs it with and without the race detector. Every gate **SHOULD** have both a positive and a
negative test. The negative half is usually the one that matters: that replay refuses an unknown
request *and did not dial*, or that a miss fails the session rather than passing quietly.

**The live agent suite** in `test/agents` runs the real clients. The unit suite drives the proxy
against bodies the tests themselves wrote, so it proves the format, the framing and the alignment
rules. What it cannot prove is that a real agent's real session plays back. Every
normalization rule in this document came from a session someone ran by hand; this is what runs them
without anyone's hands.

### 11.6 The live agent suite

One cassette per agent, login and surface, recorded from a real session and committed:

| Scenario | Client | Signed in with | Surface |
|---|---|---|---|
| `claude-code-subscription` | Claude Code | a Claude Pro/Max subscription | `anthropic.messages` |
| `claude-code-api-key` | Claude Code | `ANTHROPIC_API_KEY` | `anthropic.messages` |
| `claude-code-fireworks` | Claude Code | `FIREWORKS_API_KEY` | `anthropic.messages` |
| `codex-chatgpt` | Codex | a ChatGPT subscription | `openai.responses` |
| `codex-api-key` | Codex | `OPENAI_API_KEY` | `openai.responses` |
| `opencode-openai` | OpenCode | `OPENAI_API_KEY` | `openai.responses` |
| `opencode-anthropic` | OpenCode | `ANTHROPIC_API_KEY` | `anthropic.messages` |
| `opencode-fireworks` | OpenCode | `FIREWORKS_API_KEY` | `openai.chat` |

A client speaks an API rather than a company, and Fireworks serves the Anthropic Messages API
alongside the OpenAI ones. That is what `claude-code-fireworks` is: the same client and the same
surface as the row above it, reaching a model Anthropic does not serve.

```bash
make fixtures          # record: real logins, real providers, spends money
make test-integration  # replay: fabricated credentials, no provider configured or reachable
make test-smoke        # the same matrix again, into its own tier: the pre-push profile
```

Recording runs on a developer's machine and never in CI. Replaying needs the agents installed and no
credentials at all, so it is what CI runs on every push. Each scenario starts cs-vcr in replay mode,
then hands the agent a fabricated credential and a base URL. The whole suite takes about twenty
seconds and costs nothing.

Both halves point the agent's `HTTP_PROXY` at the same cs-vcr, so the hosts it contacts on its own
are refused identically either way (R30). That is what lets a session recorded under a real
subscription replay under a fabricated one.

A scenario this host cannot cover is skipped with the reason, and the reason names what it wanted:

```console
$ make fixtures
--- PASS: TestRecordFixtures/claude-code-subscription (4.25s)
--- SKIP: TestRecordFixtures/claude-code-api-key (0.09s)
        claude-code-api-key cannot be recorded here: ANTHROPIC_API_KEY is not set in this environment
```

**R51.** Recording **MUST** skip a scenario this host cannot sign in for, and **MUST** say which
credential was missing. *A suite that fails for want of a login is one contributors learn to ignore.*

**R51a.** Recording **MUST** prove a credential works before it opens a cassette, by running the
agent against its real provider with no cs-vcr in the way. *A credential that is present is not one
that works. A key can be revoked, and a subscription login carries no expiry anyone can read. A stale
one surfaced partway through a run, with the scenarios before it already paid for. Proven first,
the committed cassette is still there to fall back on.*

**R52.** A recorded cassette **MUST** be scrubbed and then replayed before it is kept. *A fixture
that cannot be played back is not a fixture. It is also what proves the scrub was safe: taking a
value out of a request changes what the entry matches on.*

**R53.** Replay **MUST** run with no provider configured at all. It **MUST** assert that no provider
was contacted, that no request missed, and that the agent did the work the prompt asked for. *A
scenario's settings say where its own upstream lives, and replay reads none of them. Serving every
step to a client that then failed is not a replayed session.*

**R54.** Replay **MUST** skip a scenario whose agent is absent, or whose version is not the one the
fixture was recorded with, and **MUST** be able to fail instead of skipping. *An agent's own version
is in its prompt, so a different build sends a different request; and a job that silently skipped its
whole matrix reports the same green as one that ran it.*

**R54a.** Replay **MUST** refuse a fixture recorded under a different normalization ruleset, before
it starts the agent, and **MUST** fail rather than skip. *An agent that is not installed is a gap in
what one host can cover. A fixture recorded under a ruleset the build no longer speaks is committed
and wrong for everybody. Refused per request instead, it arrives as an error the agent retries, and
the run reports a timeout rather than a cause.*

An agent builds its prompt from what it can see, so the two runs have to see the same things. Three
mechanisms, in the order they matter:

1. **The agent is denied every network path except cs-vcr.** Claude Code asks a profile endpoint who
   is signed in, and Codex asks for the connectors and plugins the account has. Those calls answer
   for a real credential and 401 for a fabricated one, and the prompt then differs by whole blocks.
   Blocked in both halves, the two runs ask the same question. It also means the recording half
   cannot rotate the developer's OAuth token.
2. **Each run gets a fresh home and agent config directory**, seeded with the same fixed identity.
   Nothing carries over from the developer's own configuration, and nothing a first run cached
   changes what a second sends.
3. **Each run is given the same explicit environment and the same flags**, so what the agent reports
   about its machine is the same sentence. Every customization that would become prompt content is
   off: memory files, skills, plugins, hooks and MCP servers.

What is left is what the ruleset in section 13 is for.

### 11.7 Coverage

Every test target writes coverage into its own tier under `.coverage/`, so running several
aggregates rather than overwrites. `make coverage` merges what is there and prints the report.

`make coverage-check` runs inside `make check` and in CI. It fails when a package
`.coverage-baseline` lists stops being reached: presence, not a percentage. What it catches is a
suite that stopped running while the tests still report green. When a package is meant to lose its
coverage, rerun `make coverage-baseline` and commit the result.

The live and cassette tiers build `cs-vcr` with `-cover`, so what the real binary runs counts too.
`test/agents` is harness, so it stays out of `-coverpkg`.

## 12. Conformance

An implementation is compliant when:

1. It satisfies every **MUST** in this document.
2. `replay` cannot reach a provider by any configuration path, proven by a test whose upstream fails
   the test if it is dialled at all.
3. A cassette it writes is readable by another compliant implementation, and the reverse.
4. A session recorded against a provider replays with `upstream calls 0` and `misses 0`.
5. Alignment gives identical verdicts for identical inputs, with no thresholds.

### 12.1 Quality attributes

```
[QA-01] Functional correctness: a recorded session replays with misses = 0
  Measured by: the record-then-replay round trip, and the live agent suite
  Classification: BEHAVIORAL

[QA-02] Cost safety: upstream calls = 0 for any replay session
  Measured by: a test upstream that fails the test if dialled
  Classification: BEHAVIORAL

[QA-03] Reviewability: a one-line prompt change produces a diff under 10 lines
  Measured by: inspection of a re-recorded cassette
  Classification: STRUCTURAL

[QA-04] Streaming fidelity: replay writes one flush per recorded SSE event
  Measured by: a ResponseWriter that counts writes and flushes
  Classification: BEHAVIORAL
```

## 13. Portability, and its one hard limit

A cassette recorded on a laptop has to replay in CI, where nothing is in the same place. Several
things in a real prompt would otherwise prevent that, all of them prose rather than fields, so no
amount of field stripping reaches them.

| What varies | Normalized to | Why it would break |
|---|---|---|
| `Today's date is 2026-08-12.`, and the spellings Codex and OpenCode use | `<DATE>` | every request misses tomorrow |
| `Primary working directory: /abs/path` | `<CWD>` | CI checks out elsewhere |
| the checkout path anywhere else, slugified or with its leading `/` gone | `<ROOT>`, `<ROOT-SLUG>`, `<ROOT-BARE>` | tool calls carry absolute paths, and a patch reports them without the slash |
| `cc_version=2.1.219.c4e`, in the billing header Claude Code sends as its first system block | `cc_version=2.1.219` | the suffix changes between runs of one binary |
| `OS Version: Linux 6.11.0-generic` | `<OS>` | the kernel release differs per host |
| `Platform: darwin` | `<PLATFORM>` | a laptop records, a Linux runner replays |
| Codex's `<timezone>Europe/Berlin</timezone>` | `<TZ>` | a runner keeps UTC |
| `The user's email address is you@example.com` | `<EMAIL>` | it is per person, and a cassette is committed |
| a per-session scratchpad uuid | `<SESSION:n>`, restored per run | a new one every session |
| the id of a chunk of tool output | `<CHUNK:n>`, restored per run | it is how a model asks for the rest |

The last two use `capture` rather than `replace`, because the agent acts on them.

**The hard limit: an agent that polls live external state is not fully replayable, and that is not a
matching problem.** An orchestrator dispatches work and then polls it. While recording, the poll
returns `finished`. On replay the same poll, run against a worker that has only just started,
returns `running`. The request differs because the agent observed a different world. No rule should
reconcile the two, because rewriting `running` to `finished` would replay a lie.

What replays cleanly is everything up to that observation. The fix, where one is wanted, belongs
outside cs-vcr: make the state deterministic, or record and replay the async step as a unit.

## 14. Open questions

1. **The look-ahead default of 8 is a judgement.** It has to exceed the number of requests a client
   can have in flight at once, and nothing establishes that bound.
2. **The drain timeout of two minutes is a judgement.** It has to exceed the time a model takes to
   compose an answer, and nothing establishes the upper bound.
3. **One session per cassette is a convention, not a check.** Nothing prevents recording two into
   one, and the effect is a script that replays with every step reported out of order.
4. **A ruleset change is not forced to bump `normalize.version`.** `cassette verify` reports the
   mismatch, but nothing makes the check run.
5. **Client-side prompt nondeterminism is unhandled by design.** Codex includes a
   `<plugins_instructions>` block on some runs and not others. Alignment reports a shape difference,
   and no rule covers it, because declaring the enclosing list volatile would blank the prompt.
