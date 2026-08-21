#!/usr/bin/env bash
# Re-record every agent fixture, from this machine's own credentials.
#
# The suite needs three API keys and two subscription logins. This finds them
# where they actually live here and hands them over; everything else — cs-vcr's
# per-scenario config, each agent's config files, the proxy settings, a fresh
# HOME per run — the suite writes itself, into a workspace it throws away. There
# is nothing for this script to create.
#
#   ./scripts/record-fixtures.sh          record all eight
#   ./scripts/record-fixtures.sh --check  say what would run, record nothing
#
# This SPENDS REAL MONEY: eight sessions against four providers. It runs
# `make fixtures-strict`, so a credential this host cannot present fails the run
# rather than skipping it — re-recording seven of eight and reporting green is
# the outcome worth refusing.
set -euo pipefail

cd "$(dirname -- "${BASH_SOURCE[0]}")/.."
repo=$PWD

check_only=0
[[ ${1:-} == --check ]] && check_only=1

fail() { printf '\n%s\n' "$*" >&2; exit 1; }
ok()   { printf '  ok    %s\n' "$*"; }
bad()  { printf '  MISS  %s\n' "$*"; }

# The three API keys, from the .env this repository already keeps them in.
# `set -a` exports what the file assigns; the file is bare VAR=value.
[[ -f .env ]] || fail ".env not found in $repo — it holds the three API keys."
set -a
# shellcheck disable=SC1091
. ./.env
set +a

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

echo "API keys (.env):"
for v in ANTHROPIC_API_KEY OPENAI_API_KEY FIREWORKS_API_KEY; do
  if [[ -n ${!v:-} ]]; then ok "$v is set"; else bad "$v is not set"; missing=1; fi
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
  fail "Something above is missing. fixtures-strict fails on the first one rather
than skipping it, so fix them before spending anything."
fi

if (( check_only )); then
  echo "--check: everything the eight scenarios need is present. Nothing recorded."
  exit 0
fi

cat <<'WARN'

This calls real providers and spends real money: eight sessions, four providers.
Re-recording REPLACES each cassette; it does not append.

WARN
read -r -p "Type 'record' to continue: " answer
[[ $answer == record ]] || fail "Nothing recorded."

echo
make fixtures-strict

cat <<'AFTER'

Recorded. Before committing, read what changed:

  git status --short
  git diff --stat cassettes/ test/agents/fixtures.json

fixtures.json carries the agent versions these were recorded against, so expect
it in the diff. Then replay them the way CI will:

  CS_VCR_AGENTS=1 CS_VCR_AGENTS_STRICT=1 \
    go test ./test/agents -run TestReplayFixtures -v -timeout 30m -count=1
AFTER
