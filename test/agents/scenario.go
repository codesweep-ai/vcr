package agents

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// mode is which half of the suite is running. It decides one thing: whether the
// agent is given the developer's real credential or a fabricated one.
type mode int

const (
	record mode = iota
	replay
)

func (m mode) String() string {
	if m == replay {
		return "replay"
	}
	return "record"
}

// scenario is one agent signed in one way, and the cassette it records into.
//
// One cassette per scenario, because a cassette is one session's script and
// these are different sessions — and because the point of the matrix is that
// each combination is exercised on its own, so a failure names the one that
// broke.
type scenario struct {
	// name is the cassette, the subtest and the manifest key.
	name string
	// bin is the agent's executable, looked up on PATH.
	bin string
	// auth says how the agent is signed in, for the skip and failure messages.
	auth string
	// model is pinned. An agent that picks its own default sends a different
	// request the day that default moves.
	model string
	// vcrConfig is this scenario's cs-vcr configuration: where the provider it
	// talks to actually lives. It is the RECORDING half's, and only its: every
	// key in it is a provider setting, and replay reads none of them.
	vcrConfig string
	// urlSuffix is what this client appends to the base URL it is given. Each
	// one appends a different amount of the API path.
	urlSuffix string
	// login says this scenario signs in with a credential FILE rather than a
	// key: a subscription, which has no token anyone could put in a variable.
	//
	// Declared rather than inferred from what the recording half found, because
	// the replay half has to write the fabricated version of the same thing on a
	// machine that has no login at all. Inferring it there produced an agent
	// started with no credential, which reports "Please run /login" and makes no
	// request — a replay that passes every assertion about the proxy and never
	// exercised it.
	login bool
	// keyFrom is the environment variable the developer's credential lives in,
	// and keyAs is the one the agent reads it from. They differ where a client
	// speaks one provider's API to another provider's endpoint: Claude Code
	// reads ANTHROPIC_API_KEY whatever is actually behind it.
	keyFrom, keyAs string

	// needs reports what the recording half must have, and why it cannot run
	// here when it does not.
	needs func(getenv func(string) string) (credential, error)
	// prepare writes the agent's configuration and credentials. It runs after
	// the proxy is up, because the port is part of the configuration.
	prepare func(sc scenario, ws *workspace, c credential, m mode, base string) error
	// command is the agent invocation, with its whole environment.
	command func(sc scenario, ws *workspace, c credential, m mode, base string) *exec.Cmd
}

// credential is what the recording half signs in with: a file the developer
// already has, and variables holding a key.
//
// The values are never written anywhere but the agent's own environment and the
// workspace, and the recorded cassette is scrubbed against them afterwards.
type credential struct {
	file string
	env  map[string]string
}

// scenarios is the matrix. Every combination that can be signed in for is here,
// whether or not this host can sign in for it: a scenario that skips says which
// credential is missing, and that is the only way a contributor learns what the
// suite would cover with one more login.
func scenarios() []scenario {
	return []scenario{
		claudeCode("claude-code-subscription", "a Claude Pro/Max subscription", "",
			"claude-sonnet-5", "https://api.anthropic.com"),
		claudeCode("claude-code-api-key", "an Anthropic API key", "ANTHROPIC_API_KEY",
			"claude-sonnet-5", "https://api.anthropic.com"),
		// Claude Code against a model that is not Anthropic's. It speaks the
		// Anthropic Messages API and Fireworks serves that API, so the client's
		// API-KEY path — a different credential, a different header, a different
		// prompt from the subscription one — is covered on a host that has no
		// Anthropic key. It also gives `anthropic.messages` a second recording
		// from a second provider, which is the surface cs-vcr routes by path.
		claudeCode("claude-code-fireworks", "a Fireworks API key", "FIREWORKS_API_KEY",
			"accounts/fireworks/models/kimi-k3", "https://api.fireworks.ai/inference"),
		codexChatGPT(),
		codexAPIKey(),
		openCode("opencode-openai", "an OpenAI API key", "openai/gpt-5.6", "openai",
			"https://api.openai.com", "OPENAI_API_KEY"),
		openCode("opencode-anthropic", "an Anthropic API key", "anthropic/claude-sonnet-5", "anthropic",
			"https://api.anthropic.com", "ANTHROPIC_API_KEY"),
		// The only scenario on the `openai.chat` surface: OpenCode is the only
		// client here that speaks it, and this is the one that does.
		openCode("opencode-fireworks", "a Fireworks API key",
			"fireworks-ai/accounts/fireworks/models/kimi-k3", "fireworks",
			"https://api.fireworks.ai/inference", "FIREWORKS_API_KEY"),
	}
}

// keyCredential is the ordinary case: a key in an environment variable, handed
// to the agent under the name that agent reads.
func keyCredential(from, as string) func(func(string) string) (credential, error) {
	return func(getenv func(string) string) (credential, error) {
		v := getenv(from)
		if v == "" {
			return credential{}, fmt.Errorf("%s is not set in this environment", from)
		}
		return credential{env: map[string]string{as: v}}, nil
	}
}

// subscriptionCredential is the Claude Code login, which is not a key: an OAuth
// token in the config directory, belonging to a Pro or Max subscription.
//
// CLAUDE_CONFIG_DIR is honoured because that is where a developer who runs
// several Claude Code configurations keeps the one they are signed in with.
func subscriptionCredential(getenv func(string) string) (credential, error) {
	dir := getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return credential{}, err
		}
		dir = filepath.Join(home, ".claude")
	}
	p := filepath.Join(dir, ".credentials.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return credential{}, fmt.Errorf("no Claude Code login at %s — run `claude` and sign in", p)
	}
	// Expiry is checked here rather than left to the agent, because the agent
	// cannot refresh: the suite denies it every network path except cs-vcr. An
	// expired token would otherwise fail the run as "not logged in", which reads
	// like the login is missing rather than stale.
	var creds struct {
		OAuth struct {
			ExpiresAt int64 `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(b, &creds); err != nil {
		return credential{}, fmt.Errorf("could not read %s: %w", p, err)
	}
	if exp := creds.OAuth.ExpiresAt; exp > 0 && time.UnixMilli(exp).Before(time.Now().Add(5*time.Minute)) {
		return credential{}, fmt.Errorf("the Claude Code login at %s expires at %s — run `claude` to refresh it",
			p, time.UnixMilli(exp).Format(time.RFC3339))
	}
	return credential{file: p}, nil
}

// Claude Code ------------------------------------------------------------

// claudeCode is one Claude Code scenario. An empty key means the subscription:
// the login is a file in the config directory, not a variable.
//
// upstream is where the Anthropic Messages API this client speaks actually
// lives. It matters only while recording — a replay session has nowhere to send
// a request — but it decides which model answered, and the model is in the
// request, so a fixture is tied to the one it was recorded against.
func claudeCode(name, auth, key, model, upstream string) scenario {
	needs, login := subscriptionCredential, true
	keyAs := ""
	if key != "" {
		needs, login, keyAs = keyCredential(key, "ANTHROPIC_API_KEY"), false, "ANTHROPIC_API_KEY"
	}
	return scenario{
		name: name, bin: "claude", auth: auth, model: model,
		login: login, keyFrom: key, keyAs: keyAs,
		// Claude Code appends the whole API path to what it is given.
		urlSuffix: "",
		vcrConfig: "providers:\n  anthropic: {base_url: " + upstream + "}\ndefault_provider: anthropic\n",
		needs:     needs,
		prepare: func(sc scenario, ws *workspace, c credential, m mode, _ string) error {
			// The account state Claude Code would otherwise build on its first
			// run: onboarding done, and a fixed identity. The identifiers decide
			// which side of a server-side experiment the client lands on, so
			// leaving them to be generated would leave the prompt to chance.
			state := map[string]any{
				"hasCompletedOnboarding": true,
				"userID":                 "cs-vcr-fixture-user",
				"machineID":              "cs-vcr-fixture-machine",
				"firstStartTime":         "2026-01-01T00:00:00.000Z",
			}
			b, err := json.Marshal(state)
			if err != nil {
				return err
			}
			if err := ws.write(".claude.json", string(b), 0o644); err != nil {
				return err
			}
			// An API key needs no file at all; it arrives in the environment.
			if !sc.login {
				return nil
			}
			if m == record {
				return ws.copyInto(".claude/.credentials.json", c.file)
			}
			return ws.write(".claude/.credentials.json", fakeClaudeLogin(), 0o600)
		},
		command: func(sc scenario, ws *workspace, c credential, m mode, base string) *exec.Cmd {
			cmd := exec.Command("claude", "-p",
				// Every customization off: a developer's CLAUDE.md, skills,
				// plugins, hooks and MCP servers are all prompt content, and none
				// of them exist on a CI runner.
				"--safe-mode",
				"--model", sc.model,
				// A fixed session id, so nothing per-run is minted for one.
				"--session-id", "5cf9d1b2-0000-4000-8000-000000000001",
				"--no-session-persistence",
				// The prompt asks for a file to be written, and a permission
				// prompt has nowhere to go in a non-interactive run.
				"--permission-mode", "acceptEdits",
				// A fixed toolset. The tools are part of the request, so an
				// agent offered a different set is asking a different question.
				"--tools", "Read", "Write")
			cmd.Env = ws.env(mergeEnv(map[string]string{
				"ANTHROPIC_BASE_URL": base,
				// Everything that is not the model call itself. Without these,
				// the client fetches experiment assignments and a profile, and
				// what comes back changes the prompt — including whether it
				// carries the account's email address.
				"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
				"DISABLE_TELEMETRY":                        "1",
				"DISABLE_ERROR_REPORTING":                  "1",
				"DISABLE_AUTOUPDATER":                      "1",
				"DISABLE_NON_ESSENTIAL_MODEL_CALLS":        "1",
			}, agentEnv(c, m)))
			cmd.Dir = ws.work
			// The prompt on stdin, because --tools takes a list and would
			// swallow a prompt written after it.
			cmd.Stdin = strings.NewReader(prompt)
			return cmd
		},
	}
}

// fakeClaudeLogin is a Pro/Max login that is not one: the shape Claude Code
// requires, with values that authenticate nothing.
//
// `scopes` is load-bearing. Claude Code checks it for the inference scope before
// it will send anything, and a credential without it fails as "Not logged in"
// — which reads as a missing fixture rather than a malformed one.
func fakeClaudeLogin() string {
	far := time.Now().Add(365 * 24 * time.Hour).UnixMilli()
	b, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":             "sk-ant-oat01-cs-vcr-replay-fixture-not-a-real-token",
			"refreshToken":            "sk-ant-ort01-cs-vcr-replay-fixture-not-a-real-token",
			"expiresAt":               far,
			"refreshTokenExpiresAt":   far,
			"scopes":                  []string{"user:inference", "user:profile"},
			"subscriptionType":        "max",
			"rateLimitTier":           "default_max_20x",
			"csVcrReplayFixtureToken": true,
		},
	})
	return string(b)
}

// Codex ------------------------------------------------------------------

// codexConfig is the provider block Codex needs, which is where its base URL
// lives: Codex takes a provider from a config file rather than an environment
// variable.
//
// include_apps_instructions is off because the connectors block is fetched with
// the account's credential: it is in the prompt when the fetch answers and
// absent when it 401s, so leaving it on records a prompt no replay can rebuild.
func codexConfig(base, model, extra string) string {
	return fmt.Sprintf(`model_provider = "vcr"
model = %q
include_apps_instructions = false

[model_providers.vcr]
name = "vcr"
base_url = %q
wire_api = "responses"
%s`, model, base, extra)
}

func codexChatGPT() scenario {
	return scenario{
		name: "codex-chatgpt", bin: "codex", auth: "a ChatGPT subscription", model: "gpt-5.6-sol",
		login: true,
		// A ChatGPT login is accepted by the ChatGPT backend, whose endpoint is
		// /responses with no version prefix — so the base URL carries none.
		urlSuffix: "",
		vcrConfig: "providers:\n  openai: {base_url: https://chatgpt.com/backend-api/codex}\ndefault_provider: openai\n",
		needs: func(getenv func(string) string) (credential, error) {
			p := codexAuthPath(getenv)
			if _, err := os.Stat(p); err != nil {
				return credential{}, fmt.Errorf("no Codex login at %s — run `codex` and sign in with ChatGPT", p)
			}
			return credential{file: p}, nil
		},
		prepare: func(_ scenario, ws *workspace, c credential, m mode, base string) error {
			if err := ws.write(".codex/config.toml",
				codexConfig(base, "gpt-5.6-sol", "requires_openai_auth = true\n"), 0o644); err != nil {
				return err
			}
			if m == record {
				return ws.copyInto(".codex/auth.json", c.file)
			}
			return ws.write(".codex/auth.json", fakeCodexLogin(), 0o600)
		},
		command: codexCommand,
	}
}

func codexAPIKey() scenario {
	return scenario{
		name: "codex-api-key", bin: "codex", auth: "an OpenAI API key", model: "gpt-5.6-sol",
		keyFrom: "OPENAI_API_KEY", keyAs: "OPENAI_API_KEY",
		// With a key the traffic goes to the versioned API, so the base URL ends
		// in /v1 and cs-vcr's openai provider stays where it points by default.
		urlSuffix: "/v1",
		vcrConfig: "providers:\n  openai: {base_url: https://api.openai.com}\ndefault_provider: openai\n",
		needs:     keyCredential("OPENAI_API_KEY", "OPENAI_API_KEY"),
		prepare: func(_ scenario, ws *workspace, _ credential, _ mode, base string) error {
			return ws.write(".codex/config.toml",
				codexConfig(base, "gpt-5.6-sol", "env_key = \"OPENAI_API_KEY\"\n"), 0o644)
		},
		command: codexCommand,
	}
}

func codexCommand(_ scenario, ws *workspace, c credential, m mode, _ string) *exec.Cmd {
	cmd := exec.Command("codex", "exec",
		// The workspace is deliberately not a git repository, so that the state
		// of one cannot reach the prompt.
		"--skip-git-repo-check",
		"--sandbox", "workspace-write",
		// No rollout files: session state on disk is what makes a second run
		// differ from a first.
		"--ephemeral",
		prompt)
	cmd.Env = ws.env(mergeEnv(map[string]string{"CODEX_HOME": filepath.Join(ws.home, ".codex")}, agentEnv(c, m)))
	cmd.Dir = ws.work
	// Codex reads stdin for extra input when it is not a terminal, and waits
	// for it to close.
	cmd.Stdin = strings.NewReader("")
	return cmd
}

func codexAuthPath(getenv func(string) string) string {
	if d := getenv("CODEX_HOME"); d != "" {
		return filepath.Join(d, "auth.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "auth.json")
}

// fakeCodexLogin is a ChatGPT login that is not one.
//
// The tokens have to be JWTs with the claims Codex reads — the account id and
// the plan — because it parses them before it sends anything, and a token it
// cannot parse ends the run at startup rather than at the first request.
func fakeCodexLogin() string {
	seg := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	const account = "00000000-0000-4000-8000-000000000000"
	jwt := seg(map[string]any{"alg": "RS256", "typ": "JWT"}) + "." + seg(map[string]any{
		"sub":   "user-cs-vcr-fixture",
		"email": "agent@example.invalid",
		"exp":   time.Now().Add(365 * 24 * time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": account,
			"chatgpt_plan_type":  "pro",
			"chatgpt_user_id":    "user-cs-vcr-fixture",
			"user_id":            "user-cs-vcr-fixture",
		},
		"https://api.openai.com/profile": map[string]any{
			"email": "agent@example.invalid", "email_verified": true,
		},
	}) + ".cs-vcr-replay-fixture-signature"
	b, _ := json.Marshal(map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token":      jwt,
			"access_token":  jwt,
			"refresh_token": "cs-vcr-replay-fixture-refresh-token",
			"account_id":    account,
		},
		"last_refresh": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	})
	return string(b)
}

// OpenCode ---------------------------------------------------------------

// openCode is one OpenCode provider. It is the only client here that reaches
// three of them, so the provider is a parameter rather than a scenario each.
func openCode(name, auth, model, provider, upstream, key string) scenario {
	// Fireworks is not a provider cs-vcr routes to by path — every OpenAI-shaped
	// request looks the same — so this cassette pins it instead, and every path
	// on the cassette goes there whatever the path says.
	config := fmt.Sprintf("providers:\n  %s: {base_url: %s}\n", provider, upstream)
	config += "default_provider: " + provider + "\n"
	if provider != "anthropic" && provider != "openai" {
		config += fmt.Sprintf("cassette_provider:\n  %s: %s\n", name, provider)
	}
	return scenario{
		name: name, bin: "opencode", auth: auth, model: model,
		// Each of these providers reads the key from the variable it is named
		// for, so the agent is handed it under the name it was found in.
		keyFrom: key, keyAs: key,
		// OpenCode ends the base URL it is given with the API version.
		urlSuffix: "/v1",
		vcrConfig: config,
		needs:     keyCredential(key, key),
		prepare: func(_ scenario, ws *workspace, _ credential, _ mode, base string) error {
			// The provider's base URL, in the project's own config file. The
			// environment variable OpenCode reads is per provider and does not
			// exist for all of them; this route works for every one.
			cfg := map[string]any{
				"$schema": "https://opencode.ai/config.json",
				"provider": map[string]any{
					providerID(model): map[string]any{"options": map[string]any{"baseURL": base}},
				},
			}
			b, err := json.MarshalIndent(cfg, "", "  ")
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(ws.work, "opencode.json"), b, 0o644)
		},
		command: func(sc scenario, ws *workspace, c credential, m mode, _ string) *exec.Cmd {
			cmd := exec.Command("opencode", "run",
				// No plugins, and no permission prompt to answer.
				"--pure", "--auto",
				"--model", sc.model,
				prompt)
			cmd.Env = ws.env(agentEnv(c, m))
			cmd.Dir = ws.work
			cmd.Stdin = strings.NewReader("")
			return cmd
		},
	}
}

// providerID is the part of an OpenCode model name before the first slash. A
// model name has slashes of its own, so the split is on the first one only.
func providerID(model string) string {
	id, _, _ := strings.Cut(model, "/")
	return id
}

// agentEnv is the credential the agent is given: the developer's while
// recording, and a fabricated one while replaying.
//
// The replay half is given a key at all because a client that finds none does
// not make the request the cassette holds — it stops at startup and asks for a
// login. What it must NOT be given is a real one, and that is the property this
// suite exists to demonstrate: the whole agent loop, for nothing, against a
// credential that could not spend a cent if the proxy did dial out.
func agentEnv(c credential, m mode) map[string]string {
	out := map[string]string{}
	for k, v := range c.env {
		if m == record {
			out[k] = v
			continue
		}
		out[k] = fakeKey(k)
	}
	return out
}

// fakeKey is a credential in the shape its provider issues, and in no way one.
func fakeKey(name string) string {
	switch name {
	case "ANTHROPIC_API_KEY":
		return "sk-ant-api03-cs-vcr-replay-fixture-not-a-real-key"
	case "FIREWORKS_API_KEY":
		return "fw_cs-vcr-replay-fixture-not-a-real-key"
	default:
		return "sk-proj-cs-vcr-replay-fixture-not-a-real-key"
	}
}

func mergeEnv(a, b map[string]string) map[string]string {
	out := map[string]string{}
	maps.Copy(out, a)
	maps.Copy(out, b)
	return out
}
