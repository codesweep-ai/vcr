# vcr

> **Record and replay the traffic between AI coding agents and LLM providers — so no API key ever
> enters a sandbox, and CI runs the whole agent loop for $0.**

[![CI](https://github.com/codesweep-ai/vcr/actions/workflows/ci.yml/badge.svg)](https://github.com/codesweep-ai/vcr/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
![Clients](https://img.shields.io/badge/clients-Claude%20Code%20%C2%B7%20Codex%20%C2%B7%20OpenCode-informational)
![Platforms](https://img.shields.io/badge/platform-Linux%20%C2%B7%20macOS-lightgrey)

`cs-vcr` is a single static binary that sits between an AI coding agent and its provider. It records
what passes through into a **cassette**: a directory you can read in a diff and commit next to your
code. Later you replay that cassette, and the run exercises the whole agent loop without calling a
provider. Nothing is spent, and nothing varies between runs.

The idea and the name come from [VCR](https://github.com/vcr/vcr) for Ruby and
[go-vcr](https://github.com/dnaeon/go-vcr). Those hook the HTTP client inside your test process.
`cs-vcr` is a separate proxy instead, because the client here is an agent binary that you cannot
link against or patch, and it often runs in another VM.

```
┌────────────────┐        ┌──────────────────────────┐        ┌──────────────┐
│  agent         │  HTTP  │  cs-vcr                  │  HTTPS │  provider    │
│                ├───────►│  identify by /c/<name>   ├───────►│  (Anthropic, │
│  keeps its own │        │  match · record · replay │        │   OpenAI,    │
│  login, sends  │◄───────┤  cassette store          │◄───────┤   Zen, …)    │
│  it unchanged  │        └──────────────────────────┘        └──────────────┘
└────────────────┘                                             (replay mode:
                                                                never reached)
```

## Quickstart

Say your build runs an agent. Put it in a script:

```bash
# build.sh
claude -p "write and test a hello world python program"
```

**Locally**, record it. You need no config file, no keys and no flags beyond the cassette name:

```bash
cs-vcr record --cassette build          # terminal 1

ANTHROPIC_BASE_URL=http://127.0.0.1:8080 ./build.sh    # terminal 2
```

Your agent keeps its own login, and nothing else about the build changes. Commit
`cassettes/build/`. **In CI**, run the same script with no provider reachable:

```bash
cs-vcr replay --cassette build
ANTHROPIC_BASE_URL=http://127.0.0.1:8080 ./build.sh
```

```
cs-vcr replay summary (cassette=build)
requests          14
replayed          14
upstream calls     0
misses             0
…
```

If the session diverged from the recording, the build fails with the reason rather than silently
calling a provider:

```
no recording for /v1/messages at step 4 of cassette "build" (client default), and `replay` never contacts a provider
step 4 was recorded as POST /v1/messages (anthropic.messages)
  messages[2].content
    recorded: Refactor the auth module
    this run: Refactor the token store
```

**A replay session starts and serves with no provider credential configured at all.** A
misconfigured CI job cannot spend money, because it has nothing to spend with.

To run several agents through one cs-vcr, give each a `/c/<name>` prefix — see
[MANUAL.md](MANUAL.md#pointing-an-agent-at-it).

## Two commands

There is no mode flag. You pick which of the two commands to run, and that decides whether the
session can spend money:

```bash
cs-vcr record --cassette build     # calls the provider, and stores what comes back
cs-vcr replay --cassette build     # serves only from the cassette, reaching nothing
```

`replay` builds a server with nowhere to send a request, so staying offline is not something you
configure. No config file, environment variable or flag can turn it into one that spends money.

## Cassettes

A cassette holds **one session as an ordered script**, in a directory of small text files you can
review in a pull request:

```
cassettes/refactor-auth/
  cassette.yaml       versions and provenance
  index.jsonl         one line per step, in the order they happened
  req/0001.json       the normalized request, pretty-printed
  resp/0001.sse       the streamed response, one SSE event per line
```

Replay serves step *N* to the *N*th request, then checks that the request that arrived is the one
recorded there. The check is exact about what the agent asked the model, and tolerant about what the
agent's tools printed back, which varies between runs.

```console
$ cs-vcr cassette ls refactor-auth         # STEP, METHOD, PATH, SURFACE, MODEL, …
$ cs-vcr cassette verify                   # pre-merge gate: contacts no provider
$ cs-vcr calibrate refactor-auth ./misses  # proposes rules from a failed replay
```

[SPEC.md](SPEC.md) is the whole story.

## Walkthroughs

The same binary serves all three clients below. In each one you point the agent at cs-vcr and leave
everything else alone, including its login. Each block runs on the defaults: cassettes in
`./cassettes`, the proxy on `127.0.0.1:8080`. Run the recorder in one terminal and the agent in
another.

### 1. Claude Code

Claude Code takes a base URL from the environment, and the Pro/Max subscription it is logged in with
keeps working.

```bash
# Terminal 1
cs-vcr record --cassette build

# Terminal 2
ANTHROPIC_BASE_URL=http://127.0.0.1:8080 claude -p "add a /version endpoint"

# Ctrl-C the recorder for its summary, then replay with nothing to spend.
cs-vcr replay --cassette build
ANTHROPIC_BASE_URL=http://127.0.0.1:8080 claude -p "add a /version endpoint"
```

### 2. Codex, signed in with ChatGPT

Codex takes its provider from a config file. Add one that points at cs-vcr:

```toml
# ~/.codex/config.toml
model_provider = "vcr"

[model_providers.vcr]
name = "vcr"
base_url = "http://127.0.0.1:8080"
wire_api = "responses"
requires_openai_auth = true       # keeps the ChatGPT login
```

A ChatGPT login is accepted by `chatgpt.com`, so send cs-vcr's OpenAI traffic there:

```yaml
# ~/.config/cs-vcr/config.yaml
providers:
  openai: {base_url: https://chatgpt.com/backend-api/codex}
default_provider: openai          # Codex opens with GET /models, which names no provider
```

```bash
cs-vcr record --cassette codex-build

# Terminal 2
codex exec "add a /version endpoint"

cs-vcr replay --cassette codex-build
codex exec "add a /version endpoint"
```

To try it without editing `~/.codex/config.toml`, pass the same settings on the command line:

```bash
codex exec -c 'model_provider="vcr"' \
  -c 'model_providers.vcr={name="vcr", base_url="http://127.0.0.1:8080", wire_api="responses", requires_openai_auth=true}' \
  "add a /version endpoint"
```

### 3. Codex, signed in with an API key

Two changes from the block above. In `~/.codex/config.toml`, ask for the key and add `/v1` to the
base URL:

```toml
base_url = "http://127.0.0.1:8080/v1"
env_key = "OPENAI_API_KEY"        # in place of requires_openai_auth
```

Then leave `~/.config/cs-vcr/config.yaml` alone: an API key is accepted by `api.openai.com`, which
is where the `openai` provider already points.

### 4. OpenCode

OpenCode takes a base URL from the environment, and ends it with `/v1`:

```bash
cs-vcr record --cassette oc-build

# Terminal 2 — an Anthropic model, then an OpenAI-shaped one.
ANTHROPIC_BASE_URL=http://127.0.0.1:8080/v1 opencode run --model anthropic/claude-sonnet-5 "add a /version endpoint"
OPENAI_BASE_URL=http://127.0.0.1:8080/v1 opencode run --model openai/gpt-5 "add a /version endpoint"
```

To pin it per project, put the same URL in `opencode.json`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": { "anthropic": { "options": { "baseURL": "http://127.0.0.1:8080/v1" } } }
}
```

## Running it

**Standalone** — a binary, anywhere:

```bash
cs-vcr replay --cassette build
```

**As a pod:** `make build-go && podman kube play deploy/vcr.yaml`, which mounts the binary you
just built. Add an agent container to the same pod and cs-vcr
becomes a sidecar. Containers in a pod share one network namespace and *nothing else*, so the agent
reaches cs-vcr on localhost while its filesystem and command line stay invisible.

If an agent runs in a sandbox, run cs-vcr outside it and give the sandbox the base URL. That needs
no special support: the same binary, the same base URL.

## A note on what a cassette contains

A cassette holds your prompts and the model's completions, and you commit it. Treat it with the same
care as the repository it lives in.

cs-vcr itself holds no credential: it forwards your agent's login upstream unchanged and never
records a request header. It does not authenticate callers either, so bind it to loopback and let
the network be the boundary.

## Docs

- [INSTALL.md](INSTALL.md) · getting the binary
- [MANUAL.md](MANUAL.md) · every command, option, file and diagnostic
- [SPEC.md](SPEC.md) · what cs-vcr guarantees, the cassette format, and how it is built
- [CONTRIBUTING.md](CONTRIBUTING.md) · working on the tool

## License

Apache 2.0 — see [LICENSE](LICENSE).
