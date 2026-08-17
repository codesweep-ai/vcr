// Package config resolves cs-vcr's configuration from a YAML file, the
// environment, and command-line flags — in that order of increasing
// precedence.
//
// There is nothing here about what a session may do. That is the command:
// `record` reaches a provider, `replay` cannot, and neither is a setting a file
// or an environment variable can quietly change.
//
// There is nothing here about credentials. cs-vcr records and replays; it does
// not authenticate callers, validate tokens, swap keys or redact anything. A
// client's credential is a header like any other, forwarded untouched, and the
// recorder never stores request headers at all — so there is no credential in
// a cassette to redact, and no key in this process to protect.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// CassettePrefix marks a base URL that names the cassette its traffic belongs
// to. `ANTHROPIC_BASE_URL=http://127.0.0.1:8080/c/refactor-auth` makes every
// request on it a step of the cassette `refactor-auth`, and running a second
// agent against a second cassette is a second base URL and nothing else.
//
// The prefix names the cassette directly. There is nothing to declare: a
// scenario exists as soon as an agent asks for it, which is what lets one
// cs-vcr serve a whole suite of them.
//
// Identity comes from the CONNECTION, so telling two agents apart costs nothing
// but a base URL and touches no credential. An agent keeps whatever login it
// already had, including a Claude Pro/Max subscription, which has no token
// anyone else could mint or check. Measured, not assumed: Claude Code 2.1.232
// with a prefixed base URL issues `POST /c/refactor-auth/v1/messages?beta=true`
// and carries the prefix on everything, including the bodiless startup probes
// that set no headers a request could have been identified by instead.
const CassettePrefix = "/c/"

// cassetteName is what a cassette may be called. It is checked rather than
// trusted because the name arrives in a URL and becomes a directory: without
// this, `/c/../../etc` is a request that reads outside the cassette store.
//
// One path segment, starting with an alphanumeric so that a name can never be
// read as a flag or a dotfile.
var cassetteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// CheckCassetteName reports whether a name may be used as a cassette.
func CheckCassetteName(name string) error {
	switch {
	case name == "":
		return errors.New("a cassette name is required")
	case len(name) > 128:
		return fmt.Errorf("cassette name is %d characters; the limit is 128", len(name))
	case !cassetteName.MatchString(name):
		return fmt.Errorf("cassette name %q must be one segment of letters, digits, dot, dash or underscore, starting with a letter or digit", name)
	}
	return nil
}

// Config is the resolved configuration.
type Config struct {
	// Cassettes is the directory holding cassette directories.
	Cassettes string `yaml:"cassettes"`
	// Cassette names the one a request goes to when its base URL names none.
	// Naming one is what asks for recording: there is no separate mode for it,
	// because "record" versus "just proxy" is not a decision, it is a
	// consequence of whether there is anywhere to record into.
	//
	// A base URL carrying CassettePrefix overrides it, which is how several
	// agents share one cs-vcr. This is the fallback, and it is what the single
	// agent deployment uses: one cassette, no prefix, no configuration.
	Cassette string `yaml:"cassette"`
	// Listen is the proxied port; Admin carries /healthz on a separate
	// listener, because an unrecognized path on the proxied one is forwarded
	// to a provider.
	Listen string `yaml:"listen"`
	Admin  string `yaml:"admin"`

	// Providers keyed by name (anthropic, openai, ...). Every upstream
	// base URL is configurable, so a corporate gateway, OpenCode Zen or a local
	// model server substitutes without a code change.
	Providers map[string]*Provider `yaml:"providers"`

	// DefaultProvider takes a request whose provider cannot be told from the
	// request itself. Claude Code opens a session with `HEAD /api/hello`, and
	// that probe carries no header at all — not anthropic-version, not
	// x-api-key, not even the SDK's user agent, because it is a bare preconnect
	// the runtime issues with a method and nothing else. Nothing in it
	// identifies where it was going. What identified it was the base URL the
	// client was pointed at, and origin mode has already thrown that away by
	// serving every provider on one listener.
	//
	// So it is configuration rather than a guess: an unidentifiable request
	// goes where this says, and a deployment that fronts a different provider
	// says so once.
	DefaultProvider string `yaml:"default_provider"`

	// CassetteProvider pins the provider for one cassette, whatever the path
	// says. It is keyed by cassette name because a cassette is reached through
	// one prefix, a prefix is a base URL, and a client configures one base URL
	// per provider — so naming the cassette has already named the provider.
	//
	// It exists for recording several agents at once. Codex opens with
	// `GET /models`, which cs-vcr does not model and which carries nothing
	// Anthropic-shaped, so without a pin it follows DefaultProvider into
	// whichever provider the other agent is using.
	//
	// Empty leaves a request to be routed by its path, then by its headers,
	// then by DefaultProvider.
	CassetteProvider map[string]string `yaml:"cassette_provider"`

	// Lookahead is how far past the expected step replay will look for an entry
	// that matches, which is what lets a client pipeline: Codex issues two
	// `GET /models` at startup and Claude Code runs title generation alongside
	// its main loop, and neither is a session that went wrong.
	//
	// A larger window cannot serve a WRONG entry — alignment is exact, so an
	// entry that aligns is this request — which is what makes a number here
	// acceptable at all. It is a search bound, not a similarity threshold: too
	// low costs a loud failure, never a quietly wrong answer. 0 is strict.
	Lookahead int `yaml:"lookahead"`

	// Normalize is the versioned strip list. It is a semantic claim
	// about what makes two requests equivalent, so it is configuration with a
	// version rather than a constant in the code.
	Normalize Normalize `yaml:"normalize"`
}

// Provider is one upstream. Only where it lives: what a client sends to reach
// it, credential included, is the client's business.
type Provider struct {
	BaseURL string `yaml:"base_url"`
}

// Normalize is the ruleset and its version. The version is recorded in every
// cassette entry so `cassette verify` can tell a stale entry from a
// changed ruleset.
type Normalize struct {
	Version int `yaml:"version"`
	// Strip are JSON paths removed before hashing.
	Strip []string `yaml:"strip_fields"`
	// Query are query parameters removed from the request target before
	// hashing, for the ones that identify the CLIENT rather than the request.
	//
	// Named one at a time, never wholesale: the query is part of the key
	// because it selects provider behaviour — Claude Code asks for beta
	// surfaces with `?beta=true` — and dropping it as a class would collapse
	// two interactions the provider answers differently into one entry.
	Query []string `yaml:"strip_query"`
	// Volatile are the JSON paths where a difference between the recorded
	// request and the live one is the WORLD answering differently rather than
	// the agent being asked something else.
	//
	// This is the line sequenced replay is built on: cs-vcr replays the model,
	// not the world. A tool call is the model's decision and must be identical;
	// a tool result is what really happened on this machine this run, which
	// cs-vcr never claimed to reproduce and deliberately lets happen for real.
	//
	// Declared by path rather than inferred, because inference here is how a
	// changed prompt gets excused as noise. A path covers everything beneath
	// it — see cassette.Rule. Nothing is volatile by default beyond what is
	// listed here.
	Volatile []string `yaml:"volatile,omitempty"`
	// Root is an absolute path replaced with <ROOT> before hashing.
	//
	// It exists because a regex cannot express "wherever this checkout lives".
	// An agent's tool calls carry absolute file paths — `{"file_path":
	// "/home/you/proj/README.md"}` — so the checkout path is threaded through
	// every follow-up request, and a cassette recorded on a laptop misses
	// everything in CI, whose checkout is somewhere else entirely.
	//
	// Defaults to the directory cs-vcr was started in, which is the repo root
	// in both places. Override with VCR_ROOT where that is not true.
	Root string `yaml:"root"`
	// Capture are patterns whose match is run-specific and must survive a
	// round trip: normalized away on the way in, put back on the way out.
	//
	// Replace is one-way and cannot do this. cs-campaign mints a
	// `dispatch-<nanoseconds>` per prompt and puts it in the prompt AND in the
	// path of a file the agent is told to open. Blanking it makes the request
	// match, and then the replayed response tells the agent to open the
	// RECORDING run's file, which does not exist. Capture blanks it for
	// matching and restores this run's value on the way back out.
	Capture []Capture `yaml:"capture,omitempty"`
	// Replace are regex substitutions applied before hashing.
	//
	// Field stripping cannot reach what actually breaks a replay: the volatile
	// parts of a real prompt are sentences, not fields. Claude Code embeds
	// `Today's date is 2026-08-12.` and `- Primary working directory: /abs/path`
	// in its system prompt, so without this a cassette recorded today misses
	// every request tomorrow, and one recorded on a laptop misses every request
	// in CI's checkout.
	Replace []Replacement `yaml:"replace"`

	compiled []compiledReplacement
	captures []compiledCapture
}

// Replacement is one regex substitution. `with` may reference capture groups as
// $1, so a rule can keep the part that identifies the sentence and blank only
// the part that varies.
type Replacement struct {
	Pattern string `yaml:"pattern"`
	With    string `yaml:"with"`
}

type compiledReplacement struct {
	re   *regexp.Regexp
	with string
}

// Capture is a run-specific value: matched by pattern, stored under a
// placeholder, restored from the live request on replay.
//
// If the pattern has a capture group, only GROUP 1 is replaced and captured —
// so group 1 goes around the run-specific VALUE, and the text that makes the
// match safe stays outside it, in a non-capturing group. That is usually what
// you want: the run-specific part is a bare identifier that would be reckless
// to match on its own. `(?:/tmp/agent/)([0-9a-f-]{36})` blanks the uuid and
// keeps the prefix; a bare uuid pattern would collapse every unrelated uuid in
// the request into one placeholder and restore them all to the same value.
type Capture struct {
	Pattern string `yaml:"pattern"`
	As      string `yaml:"as"`
}

type compiledCapture struct {
	re *regexp.Regexp
	as string
}

// apply blanks every occurrence and reports what each stood for.
//
// A pattern can match SEVERAL distinct values in one request, and they are not
// interchangeable: an orchestrator delegating to a member holds its own dispatch
// id and the one it minted for the member. Collapsing both into one placeholder
// restores them to the same value, and the replayed orchestrator then polls a
// session it never prompted — which is a stall, not a miss, and so does not even
// show up as a cassette failure.
//
// So each distinct value gets its own placeholder, numbered by order of first
// appearance: <DISPATCH:1>, <DISPATCH:2>. Order of first appearance is stable
// between a recording and its replay because the conversation is the same.
func (c compiledCapture) apply(b []byte) (map[string]string, []byte) {
	locs := c.re.FindAllSubmatchIndex(b, -1)
	if len(locs) == 0 {
		return nil, b
	}
	// Group 1 where the pattern has one, the whole match otherwise: the
	// run-specific part is usually a bare identifier, and the text around it is
	// what makes matching it safe.
	g := 0
	if len(locs[0]) >= 4 && locs[0][2] >= 0 {
		g = 1
	}
	seen := map[string]string{}
	got := map[string]string{}
	var out []byte
	last := 0
	for _, m := range locs {
		start, end := m[2*g], m[2*g+1]
		if start < 0 {
			continue
		}
		val := string(b[start:end])
		ph, ok := seen[val]
		if !ok {
			ph = c.placeholder(len(seen) + 1)
			seen[val] = ph
			got[ph] = val
		}
		out = append(out, b[last:start]...)
		out = append(out, ph...)
		last = end
	}
	out = append(out, b[last:]...)
	return got, out
}

// placeholder numbers one capture, keeping the angle brackets outside so the
// result still reads as a placeholder: <DISPATCH:2>.
func (c compiledCapture) placeholder(n int) string {
	if strings.HasSuffix(c.as, ">") {
		return c.as[:len(c.as)-1] + ":" + strconv.Itoa(n) + ">"
	}
	return c.as + ":" + strconv.Itoa(n)
}

// Captured is what one request's captures matched, carried from the request to
// the response that answers it.
type Captured map[string]string

// slug is a path with its separators turned into dashes, the convention tools
// use to derive a per-directory state path.
func slug(path string) string { return strings.ReplaceAll(path, "/", "-") }

// bare is a path with its leading separator removed, which is how a patch
// reports the files it touched. It is empty for a root shallow enough that the
// remainder would be a word rather than a path, because substituting a word
// would rewrite prose that happens to contain it.
func bare(path string) string {
	rest := strings.TrimPrefix(path, "/")
	if !strings.Contains(rest, "/") {
		return ""
	}
	return rest
}

// StripFields satisfies the ruleset the cassette package normalizes against,
// so that package needs no dependency on this one.
func (n *Normalize) StripFields() []string { return n.Strip }

// StripQuery satisfies the same ruleset, for the request target.
func (n *Normalize) StripQuery() []string { return n.Query }

// VolatilePaths are the paths where a difference is the world answering
// differently rather than the agent diverging. Returned as plain strings: this
// package describes the ruleset, and the cassette package is what knows they
// are alignment rules.
func (n *Normalize) VolatilePaths() []string { return n.Volatile }

// Compile prepares the replacements. Called once at startup so a bad pattern is
// a startup error rather than a per-request surprise.
func (n *Normalize) Compile() error {
	n.compiled = nil
	n.captures = nil
	for i, c := range n.Capture {
		if c.As == "" {
			return fmt.Errorf("normalize.capture[%d]: `as` is required — it is the placeholder the value is restored into", i)
		}
		re, err := regexp.Compile(c.Pattern)
		if err != nil {
			return fmt.Errorf("normalize.capture[%d]: %w", i, err)
		}
		n.captures = append(n.captures, compiledCapture{re: re, as: c.As})
	}
	for i, r := range n.Replace {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return fmt.Errorf("normalize.replace[%d]: %w", i, err)
		}
		n.compiled = append(n.compiled, compiledReplacement{re: re, with: r.With})
	}
	return nil
}

// Apply runs the substitutions over canonical request text.
//
// The root goes first and is a literal replacement, not a regex: a filesystem
// path is full of characters a regex would read as syntax, and quoting it into
// a pattern only invites the escaping bug.
// It also reports what the captures matched, so the response answering this
// request can have this run's values put back into it.
func (n *Normalize) Apply(b []byte) ([]byte, map[string]string) {
	b = n.ApplyRoot(b)
	var got map[string]string
	for _, c := range n.captures {
		vals, blanked := c.apply(b)
		b = blanked
		for ph, v := range vals {
			if got == nil {
				got = map[string]string{}
			}
			got[ph] = v
		}
	}
	for _, c := range n.compiled {
		b = c.re.ReplaceAll(b, []byte(c.with))
	}
	return b, got
}

// ApplyResponse normalizes a response for storage, using the placeholders THIS
// REQUEST's captures produced.
//
// It cannot number them independently. A request and the response answering it
// mention the same values in different orders — an orchestrator says "my
// dispatch is A, I prompted worker B" and the reply says "poll B, then finish
// A" — so numbering each side by its own order of first appearance swaps them,
// and replay hands the client two identifiers with their meanings exchanged.
// The request defines the mapping; the response is blanked with it.
//
// The one-way Replace rules are deliberately NOT applied here. `<DATE>` can
// never become a date again, which is fine for a request and wrong for a
// response the agent acts on.
func (n *Normalize) ApplyResponse(b []byte, captured map[string]string) []byte {
	b = n.ApplyRoot(b)
	// Longest value first, so one value that is a prefix of another cannot
	// partly rewrite it.
	phs := make([]string, 0, len(captured))
	for ph := range captured {
		phs = append(phs, ph)
	}
	sort.Slice(phs, func(i, j int) bool { return len(captured[phs[i]]) > len(captured[phs[j]]) })
	for _, ph := range phs {
		b = bytes.ReplaceAll(b, []byte(captured[ph]), []byte(ph))
	}
	return b
}

// RestoreResponse puts this run's values back into a recorded response, so the
// client acts on paths and identifiers that exist here rather than on the
// recording machine's.
func (n *Normalize) RestoreResponse(b []byte, got map[string]string) []byte {
	b = n.RestoreRoot(b)
	return Captured(got).Restore(b)
}

// Restore substitutes each captured value for its placeholder.
func (c Captured) Restore(b []byte) []byte {
	for placeholder, value := range c {
		b = bytes.ReplaceAll(b, []byte(placeholder), []byte(value))
	}
	return b
}

// ApplyRoot replaces the checkout path with placeholders, and is applied to
// RESPONSES as well as requests.
//
// It has to be. An agent's tool call names an absolute file path, cs-vcr
// replays the recorded response, and the client echoes that path straight back
// in its next request — so a response carrying the recording machine's paths
// hands them to a client that then cannot match anything. Recording the
// placeholder and restoring it on the way out makes a cassette portable between
// checkouts without the client ever seeing a path that does not exist.
//
// A placeholder per form, not one for all three, because the reverse has to be
// unambiguous: a path, its slugified form and its bare form must each come back
// as themselves.
//
// The bare form is the leading separator taken off, which is how tools report a
// path they have written: `apply_patch` answers "A home/you/proj/hello.txt", and
// tar and diff do the same. It is substituted last, because until the absolute
// form has gone every absolute path also contains its own bare form.
func (n *Normalize) ApplyRoot(b []byte) []byte {
	if n.Root == "" || n.Root == "/" {
		return b
	}
	b = bytes.ReplaceAll(b, []byte(slug(n.Root)), []byte(rootSlugPlaceholder))
	b = bytes.ReplaceAll(b, []byte(n.Root), []byte(rootPlaceholder))
	if v := bare(n.Root); v != "" {
		b = bytes.ReplaceAll(b, []byte(v), []byte(rootBarePlaceholder))
	}
	return b
}

// RestoreRoot is ApplyRoot inverted, for a response on its way to a client.
func (n *Normalize) RestoreRoot(b []byte) []byte {
	if n.Root == "" || n.Root == "/" {
		return b
	}
	b = bytes.ReplaceAll(b, []byte(rootSlugPlaceholder), []byte(slug(n.Root)))
	if v := bare(n.Root); v != "" {
		b = bytes.ReplaceAll(b, []byte(rootBarePlaceholder), []byte(v))
	}
	return bytes.ReplaceAll(b, []byte(rootPlaceholder), []byte(n.Root))
}

// The three placeholders are distinct texts rather than one with a suffix, so
// that restoring one cannot eat another: `<ROOT>` does not occur inside
// `<ROOT-SLUG>` or `<ROOT-BARE>`.
const (
	rootPlaceholder     = "<ROOT>"
	rootSlugPlaceholder = "<ROOT-SLUG>"
	rootBarePlaceholder = "<ROOT-BARE>"
)

// Default is the configuration before any file, environment or flag is
// applied. The provider set is populated because a proxy that knows no
// providers is useless, and these are the ones that must be routed.
func Default() *Config {
	return &Config{
		Cassettes: "cassettes",
		Listen:    "127.0.0.1:8080",
		Admin:     "127.0.0.1:8081",
		Providers: map[string]*Provider{
			"anthropic": {BaseURL: "https://api.anthropic.com"},
			"openai":    {BaseURL: "https://api.openai.com"},
		},
		DefaultProvider: "anthropic",
		Lookahead:       8,
		Normalize: Normalize{
			// The version is a counter, not a claim about how many rulesets exist:
			// it is recorded in every cassette so that changing a rule refuses the
			// recordings made under the old ones instead of silently missing them.
			// Bump it whenever anything below changes, and re-record.
			Version: 6,
			// The minimum names: markers and identifiers that change
			// between two requests the model would answer identically.
			Strip: []string{
				"metadata.user_id",
				"messages[].content[].cache_control",
				"system[].cache_control",
				"tools[].cache_control",
				// Codex opens every request with a telemetry block, and every
				// field in it is minted per run: session, thread, turn and
				// window ids, the installation uuid, and a nested
				// x-codex-turn-metadata string carrying the checkout's git
				// commit and a turn start in unix milliseconds.
				//
				// Wholesale, unusually — the named-one-at-a-time rule that
				// governs strip_query has nothing to bite on here, because
				// there is no field in the object that is not the client
				// identifying itself. What the model is asked lives in `input`.
				"client_metadata",
				// The session uuid again, under the name the provider caches
				// prefixes by. A cache hint changes latency and cost, never the
				// answer, which is what makes it the same case as
				// metadata.user_id rather than a query parameter that selects
				// behaviour.
				"prompt_cache_key",
			},
			// What the world answers, in each surface's spelling. Both name a
			// tool RESULT and neither names a tool call: the call is the
			// model's decision, replayed verbatim, and must match exactly.
			//
			// Measured: every replay failure of a multi-turn Codex session was
			// at the first path here — the shell's own timing, the id of an
			// output chunk, and a pyenv warning the login shell emitted on one
			// run and not the next.
			Volatile: []string{
				// OpenAI responses: what a tool call answered, in both shapes
				// that surface uses — a list of typed blocks for a custom
				// tool, a plain string for a function call. The path names
				// the field rather than a shape beneath it, because a rule
				// written for one tolerates the other not at all: OpenCode's
				// patch tool answers with a string, and a cassette recorded
				// on one machine then missed on every other.
				"input[].output",
				// Anthropic messages: a tool_result block's content
				"messages[].content[].content",
			},
			// Codex asks for the model list at startup and names its own build
			// in the query: `GET /v1/models?client_version=0.145.0`. The
			// answer does not depend on it, but the key does, so a cassette
			// recorded before an update misses on the first request after one
			// — and a miss on the model list fails the session as surely as a
			// miss on a prompt.
			Query: []string{"client_version"},
			// All of these are in the prompt text, so no amount of field
			// stripping reaches them, and all break replay in the ordinary
			// case rather than a corner one: the date changes overnight, CI
			// never checks out to the path the recording was made in, and the
			// billing suffix changes between two runs on one machine.
			Replace: []Replacement{
				{Pattern: `(Today's date is )\d{4}-\d{2}-\d{2}`, With: `${1}<DATE>`},
				{Pattern: `(Primary working directory: )[^\s\\"]+`, With: `${1}<CWD>`},
				// The same date, in the shape Codex writes it: an
				// <environment_context> block with the day in a tag of its own.
				// Two clients, two spellings, one defect — a cassette recorded
				// today that misses tomorrow.
				{Pattern: `(<current_date>)\d{4}-\d{2}-\d{2}`, With: `${1}<DATE>`},
				// And the third spelling, OpenCode's: a runtime's own
				// toDateString(). Three clients, three renderings of the same
				// fact, and each one on its own is enough to make every request
				// of a session miss tomorrow — or in an hour, for a machine
				// whose day has not turned over yet.
				{Pattern: `(Today's date: )\w{3} \w{3} \d{1,2} \d{4}`, With: `${1}<DATE>`},
				// And the machine's zone, from the same block. It is the other
				// half of the clock: a session recorded in Berlin misses every
				// request in a CI runner that keeps UTC, whatever the date says.
				{Pattern: `(<timezone>)[^<]+`, With: `${1}<TZ>`},
				// Claude Code sends its billing header as the FIRST system
				// block of every request:
				//   x-anthropic-billing-header: cc_version=2.1.219.c4e; ...
				// The trailing component after the semantic version is not the
				// version — it varies between two runs of the same client
				// binary, and differs per surface within one run (the main
				// loop, title generation and the summarizer each get their
				// own). Measured on two recordings of one task three minutes
				// apart: `.c4e`/`.e21` became `.ab2`/`.f8e`.
				//
				// It rides in the request BODY, not in a header, so "request
				// headers are never recorded" does not save us: it is hashed
				// like any other prompt text. Left alone it makes every Claude
				// Code request miss on replay, which is the whole product.
				{Pattern: `(cc_version=\d+\.\d+\.\d+)\.[0-9a-z]+`, With: `${1}`},
				// The rest of that environment block: which machine the agent
				// was invoked on. `OS Version` is the kernel release, so it
				// differs between two hosts running the same distribution and
				// changes under one host on an update. `Platform` is what lets
				// a cassette recorded on a laptop replay in a Linux CI job.
				//
				// Each ends at the newline, which in a canonical body is a
				// backslash followed by an `n` — hence the excluded backslash.
				{Pattern: `(OS Version: )[^\n"\\]+`, With: `${1}<OS>`},
				{Pattern: `(Platform: )[^\n"\\]+`, With: `${1}<PLATFORM>`},
				// The account the agent is signed in as. Claude Code puts the
				// address in a system reminder at the head of the first user
				// message, so it is prompt text like the date and the working
				// directory — and it is per-PERSON rather than per-machine. Without
				// this, a cassette one developer records misses for every other
				// one, and carries their address into the repository it is
				// committed to.
				{Pattern: `(The user's email address is )[^@\s"\\]+@[^\s"\\]+`, With: `${1}<EMAIL>`},
			},
			// The scratchpad directory Claude Code is told to use carries a
			// per-session uuid, so it changes on every run:
			//   /tmp/claude-1000/<repo-slug>/b2640482-…-b66b2a1191ea/scratchpad
			//
			// Capture rather than Replace, for the reason capture exists: the
			// agent WRITES there. Blanking it one-way would make the request
			// match and then hand the replayed agent the recording run's
			// scratchpad path, which does not exist here.
			Capture: []Capture{{
				Pattern: `(?:/tmp/claude-\d+/[^/"]+/)([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`,
				As:      "<SESSION>",
			}, {
				// The id Codex gives one chunk of a truncated tool output,
				// minted per run and carried back in the next request.
				//
				// It survives the move to sequenced replay for one reason, and
				// it is not matching: `input[].output` is volatile, so
				// the request would align without this. It is here to CORRELATE
				// — the id is how a model asks for the rest of a truncated
				// output, and a one-way blank would hand the replayed model the
				// recording run's id, naming a chunk this run does not have.
				//
				// Capture reaches it and a path cannot: the id sits inside a
				// JSON-encoded string at that leaf, not in a field of its own.
				// Measured on a real session, the model never named a chunk —
				// the output was small enough not to be truncated — so this is
				// insurance against a case that is in the protocol and has not
				// been seen yet.
				Pattern: `(?:chunk_id\\?":\\?")([^"\\]+)`,
				As:      "<CHUNK>",
			}},
		},
	}
}

// Load reads the config file at path, applying it over Default. A missing file
// is not an error: cs-vcr must run with no configuration at all, because the
// contract requires a replay session to start with nothing configured.
func Load(path string) (*Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	// KnownFields so a typo'd key is an error rather than a setting that
	// silently does nothing — the failure mode this config most invites is
	// "I set it and it was ignored".
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

// ApplyEnv overlays the environment. Only settings a deployment needs to vary
// per run are here; anything else belongs in the file.
func (c *Config) ApplyEnv(getenv func(string) string) error {
	if v := getenv("VCR_CASSETTE"); v != "" {
		c.Cassette = v
	}
	if v := getenv("CS_VCR_CASSETTES"); v != "" {
		c.Cassettes = v
	}
	if v := getenv("VCR_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := getenv("VCR_ADMIN"); v != "" {
		c.Admin = v
	}
	// The checkout root, defaulted to where cs-vcr was started. Doing it here
	// rather than in Default() keeps Default() free of process state, which is
	// what lets a test construct a config without inheriting the test runner's
	// directory.
	if v := getenv("VCR_ROOT"); v != "" {
		c.Normalize.Root = v
	} else if c.Normalize.Root == "" {
		if wd, err := os.Getwd(); err == nil {
			c.Normalize.Root = wd
		}
	}
	return nil
}

// Resolve validates the configuration. It is the last step before the proxy
// runs.
func (c *Config) Resolve() error {
	for name, p := range c.Providers {
		if p.BaseURL == "" {
			return fmt.Errorf("provider %s: base_url is required", name)
		}
	}
	if err := c.Normalize.Compile(); err != nil {
		return err
	}
	if c.Cassette != "" {
		if err := CheckCassetteName(c.Cassette); err != nil {
			return err
		}
	}
	return c.resolvePins()
}

// resolvePins checks the provider pins before the first request, so a typo
// fails at startup rather than as a 502 partway through a recording session.
func (c *Config) resolvePins() error {
	for name, provider := range c.CassetteProvider {
		if err := CheckCassetteName(name); err != nil {
			return fmt.Errorf("cassette_provider: %w", err)
		}
		if _, ok := c.Providers[provider]; !ok {
			return fmt.Errorf("cassette_provider %s: no provider named %q is configured", name, provider)
		}
	}
	return nil
}

// ProviderFor is the provider pinned for a cassette, or "" where none is.
func (c *Config) ProviderFor(cassette string) string {
	if cassette == "" {
		return ""
	}
	return c.CassetteProvider[cassette]
}

// RouteCassette reads a request path and answers two things: the cassette the
// request belongs in, and the path upstream should see.
//
// A path under CassettePrefix names its own cassette. Anything else belongs to
// the session's, so the single-agent deployment needs no prefix and no
// configuration at all.
//
// The name is checked rather than trusted, because it arrives in a URL and is
// about to become a directory. An unusable one is reported rather than quietly
// falling back to the session's cassette: a request that asked for one cassette
// and was answered from another is the failure this mechanism exists to prevent.
func (c *Config) RouteCassette(path string) (name, rest string, err error) {
	named, rest, prefixed := splitCassettePath(path)
	if !prefixed {
		return c.Cassette, path, nil
	}
	if err := CheckCassetteName(named); err != nil {
		return "", "", err
	}
	return named, rest, nil
}

// splitCassettePath takes the cassette name off a path, on a segment boundary
// so that /c/featurex is not read as the cassette `feature`.
//
// A bare prefix reports an empty name rather than no prefix at all, so a base
// URL ending in /c/ is refused as the mistake it is instead of quietly becoming
// the session's cassette.
func splitCassettePath(path string) (name, rest string, prefixed bool) {
	if !strings.HasPrefix(path, CassettePrefix) {
		return "", path, false
	}
	after := path[len(CassettePrefix):]
	if slash := strings.IndexByte(after, '/'); slash >= 0 {
		return after[:slash], after[slash:], true
	}
	// Nothing after the name: a request for the provider's own root.
	return after, "/", true
}
