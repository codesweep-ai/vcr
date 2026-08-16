# cs-vcr(1) — manual

## Name

**cs-vcr** — record and replay the traffic between an AI coding agent and its LLM provider.

## Synopsis

```
cs-vcr record   [--cassette NAME] [--cassettes DIR] [--listen ADDR] [--admin ADDR]
cs-vcr replay   [--cassette NAME] [--cassettes DIR] [--listen ADDR] [--admin ADDR]
                [--dump-misses DIR]
cs-vcr cassette ls [NAME] [--json]
cs-vcr cassette show NAME STEP
cs-vcr cassette verify [NAME...]
cs-vcr cassette scrub [NAME...] [--force] [--from-env VAR,...]
cs-vcr cassette prune NAME [--force]
cs-vcr calibrate NAME MISSDIR
cs-vcr config
cs-vcr manual
cs-vcr version

Global: [--config FILE] [-v|--verbose] [-q|--quiet] [--log-json]
```

## Description

cs-vcr is an HTTP proxy that you put between an agent and its provider. It records a session into a
**cassette**, a directory of text files you commit alongside your code, and replays that cassette
later so the run makes no provider calls. Point the agent at it with a base URL, and nothing else
about the agent changes: it keeps its own login, which cs-vcr forwards untouched.

`record` calls the provider and writes each interaction into the cassette. `replay` serves that
cassette back and reaches no provider at all, so a CI job can run the whole agent loop for nothing.

A cassette holds one session as an ordered script. Replay serves step *N* to the *N*th request, then
checks that the request that arrived is the one recorded there. The check is exact about what the
agent asked the model, and tolerant about what the agent's tools printed back.

For what cs-vcr guarantees and why it is built this way, see [SPEC.md](SPEC.md).

## Commands

### record

```
cs-vcr record --cassette NAME
```

Proxies to real providers and appends every interaction to the cassette, in order. It consults the
cassette for nothing: each call the session makes, including one it makes twice, reaches the
provider.

Stop it with Ctrl-C. It waits for responses still arriving, because each one is a step not yet
written, then prints a summary. A second Ctrl-C stops the wait.

### replay

```
cs-vcr replay --cassette NAME [--dump-misses DIR]
```

Serves only from the cassette. The server is built with nowhere to send a request, so no
configuration can make it spend money.

Exits **4** if any request had no recording. With `--dump-misses`, each missed request is written to
`DIR/<step>.json`, named after the step it was compared against, ready for `diff` or `calibrate`.

### cassette

```
cs-vcr cassette ls                  # every cassette in the store
cs-vcr cassette ls NAME             # the steps in one, in session order
cs-vcr cassette show NAME 3         # one step: metadata, request, response
cs-vcr cassette verify              # check every cassette against the current ruleset
cs-vcr cassette scrub NAME          # report the credentials and addresses it holds
cs-vcr cassette prune NAME --force  # delete body files no step references
```

`ls` and `show` read a cassette whatever versions it carries, so you can inspect one that `record`
and `replay` refuse. `verify` is the pre-merge gate: it contacts no provider and exits non-zero when
anything is stale.

`show` takes a step number or a hash prefix.

`scrub` is the step between recording a session and committing it. It scans every file for known
credential shapes and email addresses, reports what it finds by kind, and exits non-zero while
anything is left. Pass `--force` and it replaces each value with a placeholder. Name your own
secrets by the variable that holds them:

```bash
cs-vcr cassette scrub build --from-env OPENAI_API_KEY,FIREWORKS_API_KEY --force
```

The value is read from the environment, not from the command line, where every process on the
machine could read it.

Taking a value out of a request changes what replay matches on. That value was going to make the
cassette replay for nobody but you, and the remedy is a `normalize` rule, which blanks it on both
sides. Replay the cassette after a scrub.

### calibrate

```
cs-vcr replay --cassette NAME --dump-misses ./misses    # fails, dumps
cs-vcr calibrate NAME ./misses                          # proposes rules
```

Compares each dumped request with the step it was compared against, and prints the paths that
differed as `normalize.volatile` configuration. It changes no file. Read the proposal and keep the
paths where what differs is something the world decides rather than something the agent asked.

### config

Prints the resolved configuration, including which file it came from. There is no credential in the
output, because cs-vcr holds none.

### manual

```
cs-vcr manual | less
```

Prints this manual, which is Markdown text held inside the binary. The copy you get is the one that
build was made from, so it describes the commands you are actually running. A machine that has
cs-vcr has the reference, with no checkout to read and no page to fetch.

## Options

| Option | Applies to | Meaning |
|---|---|---|
| `--cassette NAME` | record, replay | The cassette this session reads and writes. |
| `--cassettes DIR` | record, replay | The directory holding cassettes. Default `./cassettes`. |
| `--listen ADDR` | record, replay | Proxied port. Default `127.0.0.1:8080`. |
| `--admin ADDR` | record, replay | Admin port, serving `/healthz`. Default `127.0.0.1:8081`. |
| `--dump-misses DIR` | replay | Write each missed request here. Off by default. |
| `--json` | cassette ls | Machine-readable output. |
| `--force` | cassette scrub, prune | Remove rather than report. |
| `--from-env VAR,...` | cassette scrub | Environment variables holding secrets to look for. |
| `--config FILE` | all | Config file path. |
| `-v`, `--verbose` | all | Debug logging. |
| `-q`, `--quiet` | all | Errors only. |
| `--log-json` | all | Structured JSON logs. |

## Pointing an agent at it

Only the base URL changes. How much of the API path the agent appends to it differs by client.

**Claude Code**

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8080 claude -p "add a /version endpoint"
```

**Codex, signed in with ChatGPT** — `~/.codex/config.toml`:

```toml
model_provider = "vcr"

[model_providers.vcr]
name = "vcr"
base_url = "http://127.0.0.1:8080"
wire_api = "responses"
requires_openai_auth = true
```

and `~/.config/cs-vcr/config.yaml`:

```yaml
providers:
  openai: {base_url: https://chatgpt.com/backend-api/codex}
default_provider: openai
```

**Codex, signed in with an API key** — the same, with two changes in `config.toml`, and no cs-vcr
config at all:

```toml
base_url = "http://127.0.0.1:8080/v1"
env_key = "OPENAI_API_KEY"
```

**OpenCode**

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8080/v1 opencode run --model anthropic/claude-sonnet-5 "…"
OPENAI_BASE_URL=http://127.0.0.1:8080/v1 opencode run --model openai/gpt-5 "…"
```

**Several agents at once.** Give each a `/c/<name>` prefix and a client entry with its own cassette:

```yaml
clients:
  - label: feature
    match: {path_prefix: /c/feature}
    cassette: refactor-auth
    provider: anthropic
```

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8080/c/feature claude
```

The prefix goes before the `/v1` where a client wants one.

## Files

| Path | What it is |
|---|---|
| `$XDG_CONFIG_HOME/cs-vcr/config.yaml` | Configuration. Optional. macOS: `~/Library/Application Support/cs-vcr/`. |
| `./cassettes/<name>/` | A cassette. Commit it. |
| `./cassettes/<name>/cassette.yaml` | Versions and provenance. |
| `./cassettes/<name>/index.jsonl` | One line per step, in order. |
| `./cassettes/<name>/req/NNNN.json` | The canonical request for step NNNN. |
| `./cassettes/<name>/resp/NNNN.json` | A non-streaming response body. |
| `./cassettes/<name>/resp/NNNN.sse` | A streamed response, one SSE event per line. |

## Environment

| Variable | Effect |
|---|---|
| `CS_VCR_CONFIG` | Config file path. |
| `CS_VCR_CASSETTES` | Cassette store directory. |
| `VCR_CASSETTE` | Cassette this session uses. |
| `VCR_LISTEN`, `VCR_ADMIN` | Listen addresses. |
| `VCR_ROOT` | Checkout root, where it is not the working directory. |

Flags beat the environment, which beats the config file.

## Exit status

| Code | Meaning |
|---|---|
| 0 | Success. |
| 1 | An ordinary error: bad configuration, a port in use, an unreadable cassette. |
| 4 | A replay session had one or more misses, or `cassette verify` or `scrub` refused. |

## Diagnostics

**`cassette_miss`** — replay had no step for a request. The message names the step it expected, the
recorded request, and the paths that disagreed. If what differs is something the world decides
rather than the agent, declare it under `normalize.volatile`, or run `calibrate` to have the rule
proposed for you.

**`unknown_client`** — the path matched no configured client. The cause is usually a base URL
missing its `/c/<name>` prefix.

**`cassette was recorded by a different build`** — the cassette's `format_version` or
`normalize_version` is not this build's. Delete it and record again. `cassette verify` shows what
changed.

**`tolerated a changed observation`** — a warning, not an error. A tool printed something different
this run, and the step was served anyway. The summary counts these as `drifted observations`.

**`served out of recorded order`** — a warning. A client pipelined, and the step was found within the
look-ahead window.

**`recording an interrupted response`** — a warning. The response did not run to its end, usually
because the client had what it wanted and hung up. What it received was recorded.

**`the response outgrew the capture limit`** — a warning. The client received the whole response,
and the cassette kept the first 64 MiB of it. That step replays truncated, so record it again.

## Making a real agent run replayable

Record, replay, run `calibrate`, read what it proposes, and keep the rules you agree with. Most of
what a real run varies is already covered by the defaults. These are the ones that need a decision:

| What varied | Rule | Why it is safe |
|---|---|---|
| a per-run identifier in the prompt *and* in a path the agent opens | `capture` | an identifier for one run, not part of the question |
| a wall clock or a pid in tool output | `volatile`, or `replace` outside a tool result | the agent reports it and does not act on it |
| a block the client includes only sometimes | `replace` | not part of the question |
| the `tools` list, when MCP servers connect on one run and not the next | `strip_fields` | see below |

The `tools` row needs thought before you copy it. A model offered different tools can answer
differently, so cs-vcr keeps the list in the key. But an agent whose toolset is not stable is not
reproducible, and interactively-authenticated MCP servers connect on some runs and time out on
others. Make the toolset deterministic, or leave it out of the key when the tools do not distinguish
the turns you are recording:

```yaml
normalize:
  strip_fields: [tools]
```

Write a `capture` pattern by enumerating the contexts an identifier appears in, such as a path, a
tool argument or a tag. Do not match its shape:

```yaml
- pattern: '(?:ID: |/tasks/|task_id":\s*"|task[_-]id>)(b[a-z0-9]{8})'
  as: '<TASK>'
```

Matching by shape would be shorter and wrong. Go's regexp is RE2 and has no lookahead, so
`b[a-z0-9]{8}` matches any nine-letter word beginning with b. In one real cassette it matched
thirteen occurrences of **behaviors** in the system prompt. Blanking those would have altered the
prompt while leaving the request looking normalized.

## Notes for agents

- Every command is non-interactive and exits with a meaningful status. Nothing prompts.
- `cs-vcr manual` prints this page from the binary. Read it there rather than looking for a file.
- `cassette ls --json` is the machine-readable listing. Everything else is line-oriented text.
- `replay` never performs network I/O to a provider, so it is safe in a sandbox with no egress.
- `calibrate` writes nothing. Its output is YAML on stdout, and it is meant to be reviewed before
  being pasted into a config file.
- To check that a proxy is up before driving an agent at it: `curl -s http://127.0.0.1:8081/healthz`
  returns `ok`.
- A cassette is committed source. Treat a change to one as you would a change to code, and re-record
  rather than editing files by hand.

## Examples

Record a build, then replay it with no provider reachable:

```bash
cat > build.sh <<'EOF'
claude -p "write and test a hello world python program"
EOF
chmod +x build.sh

cs-vcr record --cassette build &
ANTHROPIC_BASE_URL=http://127.0.0.1:8080 ./build.sh
kill -INT %1

cs-vcr replay --cassette build &
ANTHROPIC_BASE_URL=http://127.0.0.1:8080 ./build.sh
kill -INT %1
```

Find out why a replay failed:

```bash
cs-vcr replay --cassette build --dump-misses ./misses
diff cassettes/build/req/0003.json misses/0003.json
cs-vcr calibrate build ./misses
```

Gate a merge on cassettes still matching the ruleset:

```bash
cs-vcr cassette verify        # exits 4 when anything is stale
```

## See also

[SPEC.md](SPEC.md) for the specification, [README.md](README.md) for the tour,
[INSTALL.md](INSTALL.md) for getting the binary, and [CONTRIBUTING.md](CONTRIBUTING.md) for working
on cs-vcr itself.
