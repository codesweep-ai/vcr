# vcr — installation

`cs-vcr` is a single static binary with no runtime dependencies. Get it, put it on your PATH, and
run it — there is nothing to configure. Then see the [README](README.md).

## 1. Install the binary

### Download a release

From the releases page grab the archive for your OS/arch (`cs-vcr_<version>_<os>_<arch>.tar.gz`)
and `checksums.txt`, verify, then install:

```bash
sha256sum -c --ignore-missing checksums.txt      # releases are checksummed + cosign-signed
tar xzf cs-vcr_*.tar.gz cs-vcr
install -m755 cs-vcr ~/.local/bin/cs-vcr         # anywhere on your PATH
```

To verify the cosign signature as well:

```bash
cosign verify-blob checksums.txt \
  --signature checksums.txt.sig --certificate checksums.txt.pem \
  --certificate-identity-regexp 'https://github.com/codesweep-ai/vcr/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

**No version has been tagged yet, so there is nothing on the releases page today.** This route
starts working at the first tag, which is what cuts the archives, the checksum file and the
signature. Until then, take one of the two below.

### Or with `go install`

```bash
go install github.com/codesweep-ai/vcr/cmd/cs-vcr@latest
```

### Or build from source

Needs **Go 1.26+**. `goreleaser` is optional and only stamps the version:

```bash
git clone https://github.com/codesweep-ai/vcr && cd vcr
make build         # -> bin/cs-vcr  (falls back to `go build` without goreleaser)
make install       # -> ~/.local/bin/cs-vcr  (override with PREFIX=)
```

However you got it, check what you installed and read what it does:

```bash
cs-vcr version
cs-vcr manual | less    # the full reference, carried inside the binary
```

## 2. Configure

Nothing has to be configured. Every setting has a default and **a missing config file is not an
error** — `cs-vcr record` and `cs-vcr replay` run with none at all.

When you do need one, it lives at `$XDG_CONFIG_HOME/cs-vcr/config.yaml` (macOS: `~/Library/
Application Support/cs-vcr/config.yaml`), and [MANUAL.md](MANUAL.md) is the reference for every
setting. To see what won:

```console
$ cs-vcr config
config file        /home/you/.config/cs-vcr/config.yaml
listen             127.0.0.1:8080
admin              127.0.0.1:8081
cassettes          cassettes
default provider   anthropic
normalize ruleset  v6 (6 strip, 1 query, 9 replace)
normalize root     /home/you/project
cassette prefix    /c/<name>

PROVIDER   BASE URL
anthropic  https://api.anthropic.com
openai     https://api.openai.com
```

There is no credential in that file or that output, because cs-vcr never holds one.

## 3. Point an agent at it

Only the base URL changes — the agent keeps whatever login it already has, including a Claude
Pro/Max subscription:

```bash
cs-vcr record
ANTHROPIC_BASE_URL=http://127.0.0.1:8080/c/build claude -p "add a /version endpoint"
```

The `/c/build` on the end names the cassette this run belongs to. Nothing declares it: `record`
creates it on the first request, and `replay` serves it.

**Where each client expects that URL to end differs**, because each appends a different amount of
the API path to it, and Codex needs a `model_providers` entry rather than an environment variable.
Ask cs-vcr for the one you want, and it prints the command to run:

```bash
cs-vcr config claude --cassette build
```

The [README walkthroughs](README.md#walkthroughs) have the recipe for each of the three, end to end.

Confirm it is working:

```console
$ curl -s http://127.0.0.1:8081/healthz
ok
$ cs-vcr cassette ls
NAME   ENTRIES  CREATED
build  14       2026-08-12T18:04:11Z
```

## 4. Run the live agent suite

The repository carries a cassette per agent, login and surface, recorded from a real session. Two
commands drive them, and the round trip is both in order:

```bash
make fixtures          # re-record: your own logins, real providers, spends money
make test-integration  # replay:    fabricated credentials, no provider reachable
```

Recording runs on a developer's machine and never in CI. Replaying needs the agents installed and no
credentials at all, so it is what CI runs on every push. `make test-smoke` is the same replay into its
own coverage tier, and is the one to run before pushing.

Each scenario starts cs-vcr in replay mode, then hands the agent a fabricated credential and a base
URL. It then checks three things: no provider was contacted, no request missed, and the agent did
the work the prompt asked for. The whole suite takes about twenty seconds and costs nothing. A scenario
whose agent is missing is skipped with the reason, as is one whose agent is a different version from
the fixture:

```console
$ make test-integration
--- PASS: TestReplayFixtures/claude-code-subscription (0.98s)
--- PASS: TestReplayFixtures/codex-chatgpt (2.10s)
--- SKIP: TestReplayFixtures/opencode-anthropic (0.00s)
```

Codex sandboxes the shell commands it runs with bubblewrap, which needs an unprivileged user
namespace. Ubuntu 24.04 denies one to any binary without an AppArmor profile of its own, and that
includes the copy Codex bundles, so install the packaged one — Codex prefers it:

```bash
sudo apt-get install -y bubblewrap        # Ubuntu 24.04 and newer
```

Where a namespace is denied outright, the replay still serves every step and the agent's tool call
is what fails. The suite says so rather than blaming the cassette.

The agents come from their own installers, at the versions the fixtures were recorded with:

```bash
make agent-versions                     # claude 2.1.232, codex 0.145.0, opencode 1.18.11
npm install -g @anthropic-ai/claude-code@2.1.232 @openai/codex@0.145.0
curl -fsSL https://opencode.ai/install | VERSION=1.18.11 bash
```

### Re-recording the fixtures

`make fixtures` calls real providers, so it spends money and needs the logins. Every scenario this
host can sign in for is recorded, scrubbed of credentials and personal data, and replayed before it
is kept. The rest are skipped, each naming what it needed:

```console
$ make fixtures
--- PASS: TestRecordFixtures/claude-code-subscription (4.25s)
--- SKIP: TestRecordFixtures/claude-code-api-key (0.09s)
        claude-code-api-key cannot be recorded here: ANTHROPIC_API_KEY is not set in this environment
```

Each scenario needs its own login or key:

| Scenario | Sign in with |
|---|---|
| `claude-code-subscription` | `claude` and a Pro/Max subscription |
| `claude-code-api-key` | `ANTHROPIC_API_KEY` in the environment |
| `claude-code-fireworks` | `FIREWORKS_API_KEY` in the environment |
| `codex-chatgpt` | `codex` signed in with ChatGPT |
| `codex-api-key` | `OPENAI_API_KEY` in the environment |
| `opencode-openai`, `opencode-anthropic` | the matching `*_API_KEY` |
| `opencode-fireworks` | `FIREWORKS_API_KEY` |

`claude-code-fireworks` reaches a Fireworks-hosted model through the Anthropic Messages API, which
Fireworks serves alongside the OpenAI ones. It is the cover for cs-vcr fronting a provider that
serves another provider's API.

Commit what it wrote — the cassettes under `cassettes/`, and `test/agents/fixtures.json`, which
records the agent versions CI installs. The suite is specified in
[SPEC.md](SPEC.md#116-the-live-agent-suite).

## Running in a container

No cs-vcr image is published. The pod spec mounts the binary you already have, so put it at
`bin/cs-vcr` first:

```bash
make build-go
chcon -t container_file_t -l s0 bin/cs-vcr   # SELinux hosts: Fedora, RHEL, CentOS
podman kube play deploy/vcr.yaml
```

`podman kube play` relabels a hostPath directory such as the cassette store, and leaves a hostPath
naming a single file alone. Without the relabel the container cannot execute the binary it was
given: the pod reports `Degraded`, the container exits 139, and it logs nothing. The `-l s0` clears
any category a previous container left on the file, which a plain `chcon` keeps. `getenforce` says
whether your host enforces SELinux, and a host that does not has no `chcon` to run.

The pod spec ships in the release archive. There is nothing to configure in it: cs-vcr holds no
credential, and in replay mode it opens no connection to a provider at all.
