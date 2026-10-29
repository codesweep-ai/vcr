#!/usr/bin/env bash
# Re-record every agent fixture this machine can sign in for, from its own
# credentials.
#
# The suite needs two subscription logins and up to three API keys. This finds them
# where they actually live here and hands them over; everything else — cs-vcr's
# per-scenario config, each agent's config files, the proxy settings, a fresh
# HOME per run — the suite writes itself, into a workspace it throws away. There
# is nothing for this script to create.
#
#   ./scripts/record-fixtures.sh          record what this host can sign in for
#   ./scripts/record-fixtures.sh --check  say what would run, record nothing
#
# This SPENDS REAL MONEY: one session for each scenario it can sign in for,
# against up to four providers. With every key present it runs
# `make fixtures-strict`, so a scenario that skips fails the run. An API key .env
# does not set skips the scenarios that need it instead, under `make fixtures`,
# and a fixture one of them is left holding from another version of its agent is
# dropped afterwards. The logins and the agents are still required: each is one
# command away, and a run without them records almost nothing.
set -euo pipefail

cd "$(dirname -- "${BASH_SOURCE[0]}")/.."
repo=$PWD

check_only=0
[[ ${1:-} == --check ]] && check_only=1

fail() { printf '\n%s\n' "$*" >&2; exit 1; }
ok()   { printf '  ok    %s\n' "$*"; }
bad()  { printf '  MISS  %s\n' "$*"; }
skip() { printf '  SKIP  %s\n' "$*"; }

# The API keys, from the .env this repository keeps them in, and from nowhere
# else. A key the shell happens to export is not one this repository chose, and
# an expired one fails the preflight rather than skipping its scenarios.
# `set -a` exports what the file assigns; the file is bare VAR=value.
unset ANTHROPIC_API_KEY OPENAI_API_KEY FIREWORKS_API_KEY
if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

# The subscription logins. The suite reads the agents' own directories, and on
# this machine the live logins are the cs- profiles the sandbox wrappers keep —
# ~/.claude and ~/.codex may exist and be stale. Both lookups are env-driven, so
# pointing them here is all it takes.
#
# Safe to export: the suite gives the agent a fresh HOME and sets CODEX_HOME to
# the workspace, so these reach the credential lookup and nothing else.
export CLAUDE_CONFIG_DIR="${CLAUDE_CONFIG_DIR:-$HOME/.cs-claude}"
export CODEX_HOME="${CODEX_HOME:-$HOME/.cs-codex}"

echo "Recording from:"
echo "  repo                $repo"
echo "  branch              $(git rev-parse --abbrev-ref HEAD)"
echo "  CLAUDE_CONFIG_DIR   $CLAUDE_CONFIG_DIR"
echo "  CODEX_HOME          $CODEX_HOME"
echo

missing=0
skipped=0

echo "API keys (.env):"
for v in ANTHROPIC_API_KEY OPENAI_API_KEY FIREWORKS_API_KEY; do
  if [[ -n ${!v:-} ]]; then ok "$v is set"
  else skip "$v is not set — the scenarios that need it are skipped"; skipped=1
  fi
done

echo
echo "Subscription logins:"
claude_cred="$CLAUDE_CONFIG_DIR/.credentials.json"
if [[ -f $claude_cred ]]; then
  # The suite refuses a token inside five minutes of expiry, and says so. Check
  # it here instead, where the fix is one `claude` away and no money is spent.
  if python3 - "$claude_cred" <<'PY'
import json, sys, time
exp = json.load(open(sys.argv[1])).get("claudeAiOauth", {}).get("expiresAt", 0) / 1000
sys.exit(0 if exp > time.time() + 300 else 1)
PY
  then ok "Claude login at $claude_cred"
  else bad "Claude login at $claude_cred is expired — run 'claude' to refresh"; missing=1
  fi
else
  bad "no Claude login at $claude_cred"; missing=1
fi

codex_cred="$CODEX_HOME/auth.json"
if [[ -f $codex_cred ]]; then
  ok "Codex login at $codex_cred"
else
  bad "no Codex login at $codex_cred — run 'codex' and sign in with ChatGPT"; missing=1
fi

echo
echo "Agents on PATH:"
for a in claude codex opencode; do
  if command -v "$a" >/dev/null; then ok "$a — $(command -v "$a")"; else bad "$a is not on PATH"; missing=1; fi
done


if (( missing )); then
  fail "A login or an agent above is missing. Fix it before spending anything:
without them the run records almost nothing."
fi

# Strict when every key is here, so a scenario that skips for any other reason
# still fails the run.
target=fixtures-strict
(( skipped )) && target=fixtures

if (( check_only )); then
  echo "--check: this would run \`make $target\`. Nothing recorded."
  exit 0
fi

cat <<'WARN'

This calls real providers and spends real money: one session for each scenario
it can sign in for.
Re-recording REPLACES each cassette; it does not append.

WARN
read -r -p "Type 'record' to continue: " answer
[[ $answer == record ]] || fail "Nothing recorded."

echo
make "$target"

# A scenario skipped for want of a key keeps the fixture it had. Once its agent
# has moved on, that fixture is obsolete: CI installs every version
# `make agent-versions` lists, the last one wins, and a cassette recorded with
# another one fails the strict replay there. Dropped, the scenario skips on
# replay for want of a fixture, until somebody with the key records it again.
python3 - <<'PY'
import json, re, shutil, subprocess

path = "test/agents/fixtures.json"
manifest = json.load(open(path))
host = {}
dropped = False
for name, fx in sorted(manifest["fixtures"].items()):
    agent = fx["agent"]
    if agent not in host:
        out = subprocess.run([agent, "--version"], capture_output=True, text=True).stdout
        m = re.search(r"\d+\.\d+\.\d+", out)
        host[agent] = m.group(0) if m else ""
    if fx["agent_version"] != host[agent]:
        del manifest["fixtures"][name]
        shutil.rmtree(f"cassettes/{name}", ignore_errors=True)
        print(f"dropped {name}: recorded with {agent} {fx['agent_version']}, "
              f"and this host has {host[agent]}")
        dropped = True
if dropped:
    with open(path, "w") as f:
        f.write(json.dumps(manifest, indent=2, ensure_ascii=False) + "\n")
PY

cat <<'AFTER'

Recorded. Before committing, read what changed:

  git status --short
  git diff --stat cassettes/ test/agents/fixtures.json

fixtures.json carries the agent versions these were recorded against, so expect
it in the diff. Then replay them the way CI will:

  CS_VCR_AGENTS=1 CS_VCR_AGENTS_STRICT=1 \
    go test ./test/agents -run TestReplayFixtures -v -timeout 30m -count=1
AFTER
