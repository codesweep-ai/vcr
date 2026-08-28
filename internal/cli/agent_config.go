package cli

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"slices"
	"strings"

	"github.com/codesweep-ai/vcr/internal/config"
)

// This file holds what cs-vcr knows about pointing an agent at it. It prints;
// it never launches an agent and never edits a file.
//
// Printing rather than wrapping is deliberate. A `cs-vcr run <agent>` wrapper
// would only work where cs-vcr owns the launch, which rules out an IDE
// extension, a long-lived interactive session, a CI job that starts the agent
// its own way, and an agent already running on another machine. What all of
// those can take is an environment variable or a config file, so cs-vcr says
// what is needed and the caller applies it however it already works.
//
// What it is really for is one rule that nothing else states in one place:
// where the prefix goes relative to the /v1 a client appends. Claude Code
// appends the version itself and must not be given one; Codex and OpenCode are
// given a base URL that already ends in /v1. So the prefix goes in the middle,
// and getting it wrong produces a 404 from the provider rather than anything
// that names the mistake.
//
// The provider segment is the other half of the prefix, and it arrives as an
// argument. It is a key into the deployment's own `providers`, so nothing here
// can derive it from the client.

// agent is one client cs-vcr can print settings for.
type agent struct {
	name string
	// title is what a reader calls this agent, which is not what they type:
	// "Claude Code" is the product, "claude" is the binary.
	title string
	// suffix is what this client expects after the prefix, which is the whole
	// reason this command exists. It takes the provider because one client can
	// reach two endpoints that differ in it: the ChatGPT backend Codex talks to
	// has no version segment, and the OpenAI API has one.
	suffix func(provider string) string
	// env are the variables that point the agent at cs-vcr. Empty for a client
	// with no base-URL variable for this provider.
	env func(provider, base string) []envVar
	// command is a runnable one-liner: the environment, the binary and the
	// flags, ready to be copied.
	command func(provider, base string) string
	// persist is the same settings as a file, for pinning them to a project.
	persist func(provider, base string) (path, body string)
	// notes are anything else true and worth knowing, printed as comments.
	notes func(provider, base string) []string
}

// envVar is one printed setting. Values are emitted bare, so a value that would
// need quoting is refused rather than printed ambiguously — see writeEnv.
type envVar struct{ name, value string }

// providerName is what the printed Codex provider block is called. Not "vcr":
// a reader scanning a config file for what put an entry there wants the tool's
// own name.
const providerName = "cs-vcr"

// prompt is the placeholder task in every printed command. It is a real
// instruction rather than "...", so the line runs as printed.
const prompt = `add a /version endpoint`

func agents() []agent { return []agent{claudeCode(), codexCLI(), openCode()} }

func claudeCode() agent {
	return agent{
		name:  "claude",
		title: "Claude Code",
		// No /v1: Claude Code appends the version itself, and a base URL that
		// already carries one produces /v1/v1/messages.
		suffix: noVersion,
		env: func(_, base string) []envVar {
			return []envVar{{"ANTHROPIC_BASE_URL", base}}
		},
		command: func(_, base string) string {
			return fmt.Sprintf("ANTHROPIC_BASE_URL=%s claude -p %q", base, prompt)
		},
		persist: func(_, base string) (string, string) {
			return ".claude/settings.json", fmt.Sprintf(`{"env": {"ANTHROPIC_BASE_URL": %q}}`, base)
		},
		notes: func(_, _ string) []string {
			return []string{
				"Claude Code appends /v1 itself, so the base URL ends at the cassette name.",
				"The Pro/Max subscription it is signed in with keeps working: cs-vcr forwards",
				"the credential untouched and never records a request header.",
			}
		},
	}
}

func codexCLI() agent {
	return agent{
		name:  "codex",
		title: "Codex",
		// The ChatGPT backend's endpoint is /responses with no version in front,
		// so a base URL for it carries none either.
		suffix: func(provider string) string {
			if isChatGPT(provider) {
				return ""
			}
			return "/v1"
		},
		command: func(provider, base string) string {
			return fmt.Sprintf(`codex exec -c 'model_provider="%[1]s"' \
  -c 'model_providers.%[1]s={name="%[1]s", base_url="%[2]s", %[3]s, wire_api="responses"}' \
  %[4]q`, providerName, base, codexAuthInline(provider), prompt)
		},
		persist: func(provider, base string) (string, string) {
			return "~/.codex/config.toml", fmt.Sprintf(`model_provider = "%[1]s"

[model_providers.%[1]s]
name = "%[1]s"
base_url = "%[2]s"
%[3]s
wire_api = "responses"`, providerName, base, codexAuthKey(provider))
		},
		notes: func(provider, _ string) []string {
			out := []string{
				"Codex has no base-URL environment variable, so the provider block is how it",
				"is pointed anywhere. The -c flags above set the same keys without touching",
				"config.toml, which is what a build switching cassettes per test wants.",
				"",
			}
			if isChatGPT(provider) {
				return append(out,
					"This is the ChatGPT-subscription form: the login travels as itself rather",
					"than as a key, and the backend's endpoint carries no version.",
					"For an OPENAI_API_KEY session, print this again with --provider openai.")
			}
			return append(out,
				"This is the API-key form. For a ChatGPT subscription, print this again with",
				fmt.Sprintf("--provider %s, the endpoint that accepts one.", chatGPTProvider))
		},
	}
}

func openCode() agent {
	return agent{
		name:   "opencode",
		title:  "OpenCode",
		suffix: withVersion,
		env: func(provider, base string) []envVar {
			if v := openCodeVar(provider); v != "" {
				return []envVar{{v, base}}
			}
			return nil
		},
		command: func(provider, base string) string {
			if v := openCodeVar(provider); v != "" {
				return fmt.Sprintf("%s=%s opencode run --model %s/<model> %q", v, base, provider, prompt)
			}
			return fmt.Sprintf("opencode run --model %s/<model> %q  # with the file below in place",
				provider, prompt)
		},
		persist: func(provider, base string) (string, string) {
			return "./opencode.json", fmt.Sprintf(`{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    %[1]q: {"options": {"baseURL": %[2]q}}
  }
}`, provider, base)
		},
		notes: func(provider, base string) []string {
			out := []string{
				"The file is the route that works for every provider. OpenCode's base-URL",
				"variable is per provider, and only some providers have one.",
				"",
				"OpenCode also reads its whole configuration from OPENCODE_CONFIG_CONTENT, so",
				"the file above can travel as one variable instead:",
				"",
				fmt.Sprintf(`  OPENCODE_CONFIG_CONTENT='{"provider":{%q:{"options":{"baseURL":%q}}}}' \`, provider, base),
				fmt.Sprintf("    opencode run --model %s/<model> %q", provider, prompt),
			}
			if openCodeVar(provider) != "" {
				return out
			}
			return append([]string{
				fmt.Sprintf("OpenCode has no base-URL variable for %s, so the file above is how it", provider),
				"is pointed here.",
				"",
			}, out...)
		},
	}
}

// noVersion and withVersion are the two fixed answers to "what does this client
// append", for the clients that give the same one whatever they are pointed at.
func noVersion(string) string   { return "" }
func withVersion(string) string { return "/v1" }

// chatGPTProvider is the entry cs-vcr ships for the endpoint a ChatGPT
// subscription is accepted at. Codex reaches it differently from the OpenAI
// API: no version segment, and a login rather than a key.
const chatGPTProvider = "chatgpt"

func isChatGPT(provider string) bool { return provider == chatGPTProvider }

// codexAuthKey is the config.toml line that says how this endpoint authenticates.
func codexAuthKey(provider string) string {
	if isChatGPT(provider) {
		return "requires_openai_auth = true"
	}
	return `env_key = "OPENAI_API_KEY"`
}

// codexAuthInline is the same setting inside a -c provider block.
func codexAuthInline(provider string) string {
	if isChatGPT(provider) {
		return "requires_openai_auth=true"
	}
	return `env_key="OPENAI_API_KEY"`
}

// openCodeVar is the base-URL variable OpenCode reads for one provider, or ""
// for a provider it has none for. The name is OpenCode's own, and it is the one
// a model carries: `anthropic` in `anthropic/claude-sonnet-5`.
func openCodeVar(provider string) string {
	switch provider {
	case "anthropic":
		return "ANTHROPIC_BASE_URL"
	case "openai":
		return "OPENAI_BASE_URL"
	}
	return ""
}

// agentNames lists what `cs-vcr config` accepts, for its usage line.
func agentNames() []string {
	out := make([]string, 0, len(agents()))
	for _, a := range agents() {
		out = append(out, a.name)
	}
	slices.Sort(out)
	return out
}

func findAgent(name string) (agent, bool) {
	for _, a := range agents() {
		if a.name == strings.ToLower(name) {
			return a, true
		}
	}
	return agent{}, false
}

// tunnelEnv is the proxy settings that send an agent's OWN reaching-out to
// cs-vcr, alongside the base URL that sends its model calls there.
//
// Printed for every agent, because the calls it covers are not the ones a base
// URL governs. Claude Code checks its OAuth session against api.anthropic.com,
// Codex reaches chatgpt.com and ab.chatgpt.com, and each does so whatever base
// URL it was given. What those answer changes the prompt, so a session that
// leaves them alone records something no other machine can replay.
//
// NO_PROXY carries cs-vcr's own host, which is what keeps the model calls off
// the tunnel and pointed straight at the base URL above.
func tunnelEnv(proxyURL string) []envVar {
	host := proxyURL
	if u, err := url.Parse(proxyURL); err == nil && u.Host != "" {
		host = u.Hostname()
	}
	direct := host
	if host != "localhost" {
		direct = host + ",localhost"
	}
	// Both cases: the agents read the upper-case names, and much of what they
	// shell out to reads the lower-case ones.
	return []envVar{
		{"HTTP_PROXY", proxyURL},
		{"HTTPS_PROXY", proxyURL},
		{"ALL_PROXY", proxyURL},
		{"NO_PROXY", direct},
		{"no_proxy", direct},
	}
}

// tunnelPrefix is the tunnel settings as a shell prefix for the runnable line,
// wrapped so a reader can see where the agent's own command starts.
//
// Printed as part of the command rather than left to the environment block
// below it, because the line above is the one people copy. A copy that records
// a session nothing can replay is the failure this whole prefix exists to
// prevent.
func tunnelPrefix(proxyURL string) string {
	vars := tunnelEnv(proxyURL)
	parts := make([]string, 0, len(vars))
	for _, v := range vars {
		parts = append(parts, v.name+"="+v.value)
	}
	// Three, then the rest, then the agent's own command: three assignments is
	// about as much as reads at a glance on one line.
	return strings.Join(parts[:3], " ") + " \\\n  " +
		strings.Join(parts[3:], " ") + " \\\n  "
}

// printAgentConfig writes what one agent needs in order to record into or
// replay from one cassette.
func printAgentConfig(out io.Writer, a agent, proxyURL, cassette, provider string, envOnly bool) error {
	base := agentBaseURL(proxyURL, provider, cassette, a.suffix(provider))

	var vars []envVar
	if a.env != nil {
		vars = a.env(provider, base)
	}
	vars = append(vars, tunnelEnv(proxyURL)...)
	if envOnly {
		return writeEnv(out, vars)
	}

	fmt.Fprintf(out, "# %s → cassette %q on the %s provider, at %s\n\n", a.title, cassette, provider, proxyURL)
	fmt.Fprintf(out, "# Run it:\n%s%s\n", tunnelPrefix(proxyURL), a.command(provider, base))

	if len(vars) > 0 {
		fmt.Fprintf(out, "\n# Or set the environment once, and run %s as you normally would:\n", a.name)
		if err := writeEnv(out, vars); err != nil {
			return err
		}
	}
	if a.persist != nil {
		path, body := a.persist(provider, base)
		fmt.Fprintf(out, "\n# Or pin it, in %s:\n%s\n", path, body)
	}
	if a.notes != nil {
		fmt.Fprintln(out)
		for _, line := range a.notes(provider, base) {
			fmt.Fprintln(out, strings.TrimRight("# "+line, " "))
		}
	}
	for _, line := range []string{
		"",
		"The proxy lines are not optional for a session you mean to replay.",
		"This agent contacts hosts of its own beyond the base URL, and what they",
		"answer changes the prompt it sends. cs-vcr refuses those on the same",
		"address and tunnels everything else, so the tools the agent runs keep",
		"their network. Set them while RECORDING as well: blocked in both halves,",
		"the two runs ask the same question.",
		"",
		"A file that pins only the base URL misses them, so pin these too or",
		"export them beside it.",
	} {
		fmt.Fprintln(out, strings.TrimRight("# "+line, " "))
	}
	return nil
}

// writeEnv prints settings as bare VAR=VALUE lines: no export, no quoting, no
// comments.
//
// One shape rather than several, because quoting is where the readers of a
// dotenv file stop agreeing. `docker --env-file` keeps the quotes as part of
// the value while most libraries strip them, so a quoted line means two
// different things depending on who reads it. An unquoted one means the same
// thing everywhere, and that holds only while no value contains whitespace — so
// a value that does is refused rather than printed in a form the reader would
// have to guess at.
func writeEnv(out io.Writer, vars []envVar) error {
	for _, v := range vars {
		if strings.ContainsAny(v.value, " \t\r\n") {
			return fmt.Errorf("%s would need quoting, which readers of a dotenv file disagree about: %q",
				v.name, v.value)
		}
		if _, err := fmt.Fprintf(out, "%s=%s\n", v.name, v.value); err != nil {
			return err
		}
	}
	return nil
}

// agentBaseURL composes the base URL for one agent, one provider and one
// cassette: the proxy, then the prefix naming both, then whatever version
// segment this client expects.
func agentBaseURL(proxyURL, provider, cassette, suffix string) string {
	return strings.TrimSuffix(proxyURL, "/") + config.Prefix + provider + "/" + cassette + suffix
}

// proxyURL is the address an agent should be pointed at, derived from what
// cs-vcr listens on.
//
// A wildcard bind is rewritten to loopback: 0.0.0.0 is what cs-vcr accepts
// connections on, and never an address anything can connect to.
func proxyURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}
