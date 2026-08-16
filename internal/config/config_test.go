package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/vcr/internal/cassette"
)

// A missing config file is not an error: cs-vcr has to run with none at all.
func TestLoadMissingFileYieldsDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("Load of a missing file: %v", err)
	}
	if c.Cassettes != "cassettes" {
		t.Errorf("cassettes = %q, want the default", c.Cassettes)
	}
}

// A typo'd key fails loudly. The failure mode this config most invites is "I
// set it and it was ignored", which is also what a stale binary does with a
// config naming a field it does not know.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("moed: replay\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a misspelled key was accepted silently")
	}
}

// Matching is on segment boundaries, and the remainder is what upstream sees.
func TestMatchClient(t *testing.T) {
	c := Default()
	c.Clients = []*Client{
		{Label: "feature", Match: ClientMatch{PathPrefix: "/c/feature"}},
		{Label: "feat", Match: ClientMatch{PathPrefix: "/c/feat"}},
	}
	cases := []struct {
		path, label, rest string
		matched           bool
	}{
		{"/c/feature/v1/messages", "feature", "/v1/messages", true},
		{"/c/feat/v1/messages", "feat", "/v1/messages", true},
		// A bare prefix is a valid request for the provider's root.
		{"/c/feature", "feature", "/", true},
		// Not a segment boundary, and not any other client either.
		{"/c/featurex/v1/messages", "", "", false},
		{"/v1/messages", "", "", false},
	}
	for _, tc := range cases {
		cl, rest, ok := c.MatchClient(tc.path)
		if ok != tc.matched {
			t.Errorf("MatchClient(%q) matched = %v, want %v", tc.path, ok, tc.matched)
			continue
		}
		if !ok {
			continue
		}
		if cl.Label != tc.label {
			t.Errorf("MatchClient(%q) label = %q, want %q", tc.path, cl.Label, tc.label)
		}
		if rest != tc.rest {
			t.Errorf("MatchClient(%q) rest = %q, want %q — upstream must see its own path", tc.path, rest, tc.rest)
		}
	}
}

// With no clients configured everything still works, so the simple deployment
// pays nothing for a feature it is not using.
func TestMatchClientWithNoneConfigured(t *testing.T) {
	c := Default()
	cl, rest, ok := c.MatchClient("/v1/messages")
	if !ok || cl != nil || rest != "/v1/messages" {
		t.Errorf("MatchClient = (%v, %q, %v), want an unnamed match with the path untouched", cl, rest, ok)
	}
}

// A catch-all client is the escape hatch for a caller that cannot be given a
// prefix — but it must be last, or the clients after it are dead config that
// reads as though it works.
func TestCatchAllMustBeLast(t *testing.T) {
	c := Default()
	c.Clients = []*Client{
		{Label: "everything"},
		{Label: "feature", Match: ClientMatch{PathPrefix: "/c/feature"}},
	}
	if err := c.Resolve(); err == nil {
		t.Fatal("a client that can never match was accepted")
	}

	c.Clients = []*Client{
		{Label: "feature", Match: ClientMatch{PathPrefix: "/c/feature"}},
		{Label: "everything"},
	}
	if err := c.Resolve(); err != nil {
		t.Fatalf("a trailing catch-all should be fine: %v", err)
	}
	if cl, _, ok := c.MatchClient("/anything/else"); !ok || cl.Label != "everything" {
		t.Errorf("the catch-all did not catch: %v %v", cl, ok)
	}
}

func TestResolveRejectsBadClients(t *testing.T) {
	cases := map[string][]*Client{
		"no label":         {{Match: ClientMatch{PathPrefix: "/c/x"}}},
		"duplicate label":  {{Label: "a", Match: ClientMatch{PathPrefix: "/c/x"}}, {Label: "a", Match: ClientMatch{PathPrefix: "/c/y"}}},
		"prefix without /": {{Label: "a", Match: ClientMatch{PathPrefix: "c/x"}}},
		"two catch-alls":   {{Label: "a"}, {Label: "b"}},
	}
	for name, clients := range cases {
		c := Default()
		c.Clients = clients
		if err := c.Resolve(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestResolveRequiresABaseURL(t *testing.T) {
	c := Default()
	c.Providers["anthropic"] = &Provider{}
	if err := c.Resolve(); err == nil {
		t.Fatal("a provider with no base_url was accepted")
	}
}

// The billing header is the first system block of every Claude Code request,
// and the component after the semantic version is not the version: two
// recordings of one task three minutes apart carried `.c4e` and `.ab2` for the
// same surface, with every other byte identical. It travels in the body, so it
// is hashed like prompt text — left alone it misses every request there is.
func TestDefaultNormalizesTheClaudeCodeBillingSuffix(t *testing.T) {
	n := Default().Normalize
	if err := n.Compile(); err != nil {
		t.Fatal(err)
	}
	run1, _ := n.Apply([]byte(`"x-anthropic-billing-header: cc_version=2.1.219.c4e; cc_entrypoint=cli;"`))
	run2, _ := n.Apply([]byte(`"x-anthropic-billing-header: cc_version=2.1.219.ab2; cc_entrypoint=cli;"`))
	if string(run1) != string(run2) {
		t.Errorf("two runs still differ:\n  %s\n  %s", run1, run2)
	}
	// The version itself must survive: a real client upgrade changes what the
	// request means and should not silently replay against the old recording.
	if !strings.Contains(string(run1), "cc_version=2.1.219") {
		t.Errorf("the version was blanked along with the suffix: %s", run1)
	}
	if other, _ := n.Apply([]byte(`"cc_version=2.2.0.c4e;"`)); string(other) == string(run1) {
		t.Errorf("2.1.219 and 2.2.0 normalized to the same thing")
	}
}

// The scratchpad path carries a per-session uuid and the agent writes there, so
// it has to round-trip: blanked for matching, this run's value restored into
// the response. A one-way replace would match and then point the replayed agent
// at the recording run's directory.
func TestDefaultRoundTripsTheSessionScratchpad(t *testing.T) {
	n := Default().Normalize
	if err := n.Compile(); err != nil {
		t.Fatal(err)
	}
	const dir = "/tmp/claude-1000/-home-me-proj/"
	run1, _ := n.Apply([]byte(dir + "b2640482-1013-445d-a9ce-b66b2a1191ea/scratchpad"))
	run2, got := n.Apply([]byte(dir + "aee9aa02-d345-4d67-8cdf-9ff19fac7b59/scratchpad"))
	if string(run1) != string(run2) {
		t.Fatalf("two sessions still differ:\n  %s\n  %s", run1, run2)
	}
	restored := n.RestoreResponse([]byte("write to "+string(run2)), got)
	if !strings.Contains(string(restored), "aee9aa02-d345-4d67-8cdf-9ff19fac7b59") {
		t.Errorf("the response was not restored to this run's session: %s", restored)
	}
}

// The rest of Claude Code's environment block: whose machine this is, and whose
// account. Measured on two real recordings — a developer laptop and a Linux CI
// runner disagree on the kernel release, and two developers disagree on the
// address in the userEmail reminder, both inside prompt text where no field
// strip reaches them.
func TestDefaultNormalizesTheMachineAndTheAccount(t *testing.T) {
	n := Default().Normalize
	if err := n.Compile(); err != nil {
		t.Fatal(err)
	}
	// The canonical form is JSON text, so the newlines are escaped and each rule
	// has to stop at one.
	block := func(platform, os, email string) string {
		return `"# Environment\n - Platform: ` + platform + `\n - OS Version: ` + os +
			`\n - Shell: unknown\n# userEmail\nThe user's email address is ` + email +
			`.\n# currentDate\nToday's date is 2026-08-15.\n"`
	}
	laptop, _ := n.Apply([]byte(block("darwin", "Darwin 25.2.0", "ada@example.com")))
	runner, _ := n.Apply([]byte(block("linux", "Linux 6.11.0-1018-azure", "grace@example.org")))
	// Codex reports the zone rather than the kernel, in a block of its own.
	zoned := func(tz string) string {
		return `"<environment_context>\n  <cwd><ROOT>/work</cwd>\n  <timezone>` + tz + `</timezone>\n"`
	}
	berlin, _ := n.Apply([]byte(zoned("Europe/Berlin")))
	utc, _ := n.Apply([]byte(zoned("UTC")))
	if string(berlin) != string(utc) {
		t.Errorf("two zones still differ:\n  %s\n  %s", berlin, utc)
	}
	// The day, in each client's own rendering of it. A machine an hour behind
	// another is on a different date for that hour, so this is not only about
	// tomorrow.
	days := [][]byte{}
	for _, form := range []string{
		`"Today's date is 2026-08-15.\n"`, `"Today's date is 2026-08-16.\n"`,
		`"<current_date>2026-08-15</current_date>"`, `"<current_date>2026-08-16</current_date>"`,
		`"  Today's date: Sat Aug 15 2026\n"`, `"  Today's date: Sun Aug 16 2026\n"`,
	} {
		out, _ := n.Apply([]byte(form))
		days = append(days, out)
	}
	for i := 0; i < len(days); i += 2 {
		if string(days[i]) != string(days[i+1]) {
			t.Errorf("two days still differ:\n  %s\n  %s", days[i], days[i+1])
		}
	}
	if string(laptop) != string(runner) {
		t.Errorf("a laptop and a CI runner still differ:\n  %s\n  %s", laptop, runner)
	}
	// Each rule stops where its value does. A greedy one would swallow the rest
	// of the prompt, which is the part that has to keep matching.
	for _, want := range []string{"Shell: unknown", "Today's date is <DATE>", "# userEmail"} {
		if !strings.Contains(string(laptop), want) {
			t.Errorf("%q was consumed along with the value it follows:\n%s", want, laptop)
		}
	}
	// And it is the value that goes, not the label: a reader of the recording
	// has to be able to see which line was normalized.
	for _, want := range []string{"Platform: <PLATFORM>", "OS Version: <OS>", "address is <EMAIL>"} {
		if !strings.Contains(string(laptop), want) {
			t.Errorf("%q is missing from the normalized form:\n%s", want, laptop)
		}
	}
}

// The shipped volatile paths have to reach what actually broke, in each
// surface's own spelling. Asserted against the real shapes rather than against
// the strings themselves, so that renaming a path in the ruleset cannot pass
// this test while ceasing to cover the thing it was written for.
func TestTheDefaultVolatilePathsCoverWhatBrokeReplay(t *testing.T) {
	n := Default().Normalize
	rules := cassette.Rules(n.VolatilePaths())

	// A Codex turn, where the shell's own timing and the id it gives an output
	// chunk are fed back into the next request.
	codex := func(wall string) string {
		return `{"model":"gpt-5.6-sol","input":[
		  {"role":"user","content":[{"type":"input_text","text":"list the files"}]},
		  {"call_id":"c1","type":"custom_tool_call","name":"exec","input":"ls internal"},
		  {"call_id":"c1","type":"custom_tool_call_output","output":[
		    {"type":"input_text","text":"Script completed\nWall time ` + wall + ` seconds\n"}]}]}`
	}
	// A Claude Code turn, where a tool_result block carries the same kind of
	// thing under another name.
	claude := func(out string) string {
		return `{"model":"claude-sonnet-5","messages":[
		  {"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"command":"ls"}}]},
		  {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"` + out + `"}]}]}`
	}

	for _, c := range []struct{ what, a, b string }{
		{"openai responses tool output", codex("0.2"), codex("0.6")},
		{"anthropic messages tool result", claude("a\\nb\\n"), claude("warning\\na\\nb\\n")},
	} {
		got, err := cassette.Align([]byte(c.a), []byte(c.b), rules)
		if err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		if !got.Matches() {
			t.Errorf("%s is not covered by the shipped ruleset: shape=%v leaf=%v",
				c.what, got.Shape, got.Leaf)
		}
		if len(got.Tolerated) != 1 {
			t.Errorf("%s: tolerated = %v, want the one difference reported", c.what, got.Tolerated)
		}
	}

	// The negative half: what the agent DECIDED is never covered. A ruleset
	// broad enough to excuse a changed tool call would replay an answer to a
	// command that was never run.
	changed := strings.Replace(codex("0.2"), `"input":"ls internal"`, `"input":"rm -rf /"`, 1)
	got, err := cassette.Align([]byte(codex("0.2")), []byte(changed), rules)
	if err != nil {
		t.Fatal(err)
	}
	if got.Matches() {
		t.Error("the shipped ruleset excuses a changed tool call")
	}
}

// A path a patch tool reports has lost its leading separator, and that form has
// to normalize too. Measured on a real OpenCode session through OpenAI:
// `apply_patch` answered "Success. Updated the following files:\nA
// home/you/.cache/…/hello.txt", so the recording machine's home directory sat
// in a committed cassette, untouched by a rule that only knew the absolute form.
func TestRootNormalizesTheFormAPatchReports(t *testing.T) {
	line := func(root string) string {
		return `"Updated: A ` + root[1:] + `/hello.txt, wrote ` + root + `/out.log"`
	}
	normalized := func(root string) []byte {
		n := Default().Normalize
		n.Root = root
		return n.ApplyRoot([]byte(line(root)))
	}
	const laptop = "/home/ada/.cache/work"
	mine, theirs := normalized(laptop), normalized("/home/runner/work/repo")
	if string(mine) != string(theirs) {
		t.Errorf("two checkouts still differ:\n  %s\n  %s", mine, theirs)
	}
	if strings.Contains(string(mine), "ada") {
		t.Errorf("the bare form kept the home directory: %s", mine)
	}

	// And it round-trips: the client is about to act on what comes back, so
	// each form has to be restored as itself rather than as another.
	n := Default().Normalize
	n.Root = laptop
	if restored := n.RestoreRoot(mine); string(restored) != line(laptop) {
		t.Errorf("restore did not invert:\n  %s\n  %s", restored, line(laptop))
	}
}

// A root one segment deep has no bare form: the remainder is a word, and
// substituting a word would rewrite any prose that used it.
func TestAShallowRootHasNoBareForm(t *testing.T) {
	n := Default().Normalize
	n.Root = "/srv"
	got := n.ApplyRoot([]byte(`"the srv directory, under /srv"`))
	if !strings.Contains(string(got), "the srv directory") {
		t.Errorf("a word was substituted as though it were a path: %s", got)
	}
	if !strings.Contains(string(got), "<ROOT>") {
		t.Errorf("the absolute form was not normalized: %s", got)
	}
}
