# Installing cs-vcr

`cs-vcr` is a single static binary with no runtime dependencies. Get it, put it on your PATH, and
run it, and there is nothing to configure. Then see the [README](README.md).

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
error**. `cs-vcr record` and `cs-vcr replay` run with none at all.

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

Only the base URL changes. The agent keeps whatever login it already has, including a Claude
Pro/Max subscription:

```bash
cs-vcr record
ANTHROPIC_BASE_URL=http://127.0.0.1:8080/c/build claude -p "add a /version endpoint"
```

For a session you mean to replay, point the agent's proxy at cs-vcr as well. A base URL aims the
model calls. These agents also contact hosts of their own, and what those answer changes the prompt
they send. `cs-vcr config <agent>` prints the settings, and
[MANUAL.md](MANUAL.md#the-calls-a-base-url-does-not-govern) says what they cover.

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
