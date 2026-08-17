package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codesweep-ai/vcr/internal/cassette"
)

// run drives the real cobra tree with a fake environment, so the tests exercise
// the command wiring rather than a reimplementation of it.
//
// The config path defaults to one that does not exist: a stray config file on
// the developer's machine would otherwise decide what these tests assert.
func run(t *testing.T, env map[string]string, args ...string) (string, error) {
	t.Helper()
	return runWithConfig(t, "", env, args...)
}

// runWithConfig is run with a config file written for the test.
func runWithConfig(t *testing.T, yaml string, env map[string]string, args ...string) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if yaml != "" {
		if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	app := &App{Getenv: func(k string) string { return env[k] }}
	cmd := newRootCmd(app)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(append([]string{"--config", path}, args...))

	// Bounded, because `serve` blocks until its context is done. A unit test
	// that accidentally reaches a listening server should fail in seconds
	// rather than hang the suite.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := cmd.ExecuteContext(ctx)
	return out.String(), err
}

func TestVersionPrints(t *testing.T) {
	out, err := run(t, nil, "version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "cs-vcr ") {
		t.Errorf("version output = %q", out)
	}
}

// The manual is embedded, so the binary answers "how do I use this" on its own.
// The assertion is that real content arrives, not a stub or an empty string: an
// embed that stopped matching MANUAL.md would otherwise pass silently.
func TestManualPrintsTheEmbeddedManual(t *testing.T) {
	out, err := run(t, nil, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "# cs-vcr(1) — manual") {
		t.Errorf("manual output starts %.40q", out)
	}
	for _, want := range []string{"## Synopsis", "cs-vcr replay", "## Exit status"} {
		if !strings.Contains(out, want) {
			t.Errorf("manual output missing %q", want)
		}
	}
}

// `cs-vcr config` is what a user checks after a base URL did not do what they
// expected, so it must show where each setting came from.
func TestConfigPrintsResolvedSettings(t *testing.T) {
	out, err := run(t, nil, "config")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PROVIDER", "api.anthropic.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("config output missing %q:\n%s", want, out)
		}
	}
}

// 's surface, asserted as a list: the commands a user or a script has
// been told exist must keep existing.
func TestCommandSurface(t *testing.T) {
	for _, args := range [][]string{
		{"record", "--help"},
		{"replay", "--help"},
		{"cassette", "ls", "--help"},
		{"cassette", "show", "--help"},
		{"cassette", "verify", "--help"},
		{"cassette", "scrub", "--help"},
		{"cassette", "prune", "--help"},
		{"config", "--help"},
		{"version", "--help"},
		{"manual", "--help"},
	} {
		if _, err := run(t, nil, args...); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// `cassette scrub` is a gate, so its answer has to be an exit code: a cassette
// still holding a credential fails the command, and the same cassette after
// --force passes it. The value it found is named by kind and never printed —
// a report is pasted into issues and logs, and one that quotes the secret has
// published it a second time.
func TestScrubGatesACassetteThenClearsIt(t *testing.T) {
	store := t.TempDir()
	const key = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	writeCassette(t, filepath.Join(store, "leaky"),
		`{"messages":[{"role":"user","content":"the key is `+key+`"}]}`)

	env := map[string]string{"CS_VCR_CASSETTES": store}
	out, err := run(t, env, "cassette", "scrub", "leaky")
	if err == nil {
		t.Fatalf("a cassette carrying a key passed the gate:\n%s", out)
	}
	var status *ExitStatus
	if !errors.As(err, &status) || status.Code != ExitCassetteMiss {
		t.Errorf("error = %v, want exit %d", err, ExitCassetteMiss)
	}
	if !strings.Contains(out, "anthropic-key") {
		t.Errorf("the finding was not named by kind:\n%s", out)
	}
	if strings.Contains(out, key) {
		t.Errorf("the report printed the secret it found:\n%s", out)
	}

	if out, err = run(t, env, "cassette", "scrub", "leaky", "--force"); err != nil {
		t.Fatalf("--force: %v\n%s", err, out)
	}
	if out, err = run(t, env, "cassette", "scrub", "leaky"); err != nil {
		t.Fatalf("a scrubbed cassette still fails the gate: %v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(store, "leaky", "req", "0001.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), key) {
		t.Errorf("the key survived --force:\n%s", b)
	}
}

// A variable that is not set in this environment is reported, not skipped in
// silence: a caller who asked for it to be looked for would otherwise read
// "clean" as "checked".
func TestScrubReportsASecretItCouldNotLookFor(t *testing.T) {
	store := t.TempDir()
	writeCassette(t, filepath.Join(store, "plain"), `{"messages":[{"role":"user","content":"hello"}]}`)
	out, err := run(t, map[string]string{"CS_VCR_CASSETTES": store},
		"cassette", "scrub", "plain", "--from-env", "DEPLOY_TOKEN")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "DEPLOY_TOKEN") || !strings.Contains(out, "not set") {
		t.Errorf("an unset variable was not reported:\n%s", out)
	}
}

// writeCassette lays down a one-step cassette with the given request body, so a
// command test has something real to read.
func writeCassette(t *testing.T, dir, request string) {
	t.Helper()
	store, err := cassette.OpenStore(dir, "test", 2, func() int64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(cassette.Recording{
		Entry:    cassette.Entry{Method: "POST", Path: "/v1/messages", Status: 200},
		Request:  []byte(request),
		Response: []byte(`{"ok":true}`),
	}); err != nil {
		t.Fatal(err)
	}
}

// A pinned provider naming one that is not configured is a typo, and it fails
// here rather than as a 502 on the first request of a recording session.
func TestConfigRejectsAPinToAnUnknownProvider(t *testing.T) {
	cfg := "cassette_provider:\n  build: anthropick\n"
	if _, err := runWithConfig(t, cfg, nil, "config"); err == nil {
		t.Fatal("a pin naming an unconfigured provider was accepted")
	}
}

// The name becomes a directory, so it is checked wherever it is written — in
// the file as well as on a base URL.
func TestConfigRejectsACassetteNameThatIsNotOne(t *testing.T) {
	if _, err := runWithConfig(t, "cassette: ../escape\n", nil, "config"); err == nil {
		t.Fatal("a traversing cassette name was accepted")
	}
}

// What a user checks after a base URL did not behave as expected: where the
// prefix goes, and which cassettes have a provider pinned.
func TestConfigPrintsThePrefixAndThePins(t *testing.T) {
	cfg := "cassette_provider:\n  codex-run: openai\n"
	out, err := runWithConfig(t, cfg, nil, "config")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/c/<name>", "codex-run", "openai"} {
		if !strings.Contains(out, want) {
			t.Errorf("config output missing %q:\n%s", want, out)
		}
	}
}

// The safety property is which command you typed, not a setting: `replay` is
// built with no way to reach a provider, so no config file, environment
// variable or flag can turn it into one that spends money.
func TestReplayAndRecordAreSeparateCommands(t *testing.T) {
	for _, args := range [][]string{{"record", "--help"}, {"replay", "--help"}} {
		out, err := run(t, nil, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if args[0] == "replay" && !strings.Contains(out, "No provider is ever") {
			t.Errorf("replay --help does not state the guarantee:\n%s", out)
		}
	}
	// And there is no flag that could blur them.
	if out, _ := run(t, nil, "--help"); strings.Contains(out, "--offline") || strings.Contains(out, "--mode") {
		t.Errorf("a flag still competes with the command:\n%s", out)
	}
}

// `cs-vcr config <agent>` is how a user finds out where the prefix goes
// relative to the /v1 a client appends, which differs by client and is the
// mistake this design invites. Asserted per client, because a single rule that
// looked right for one of them is exactly the bug.
func TestConfigPrintsHowToPointEachAgentAtACassette(t *testing.T) {
	cases := []struct{ agent, wantBase string }{
		// Claude Code appends /v1 itself, so the base URL stops at the name.
		{"claude", "http://127.0.0.1:8080/c/build"},
		// Codex and OpenCode are given a base URL that already carries it.
		{"codex", "http://127.0.0.1:8080/c/build/v1"},
		{"opencode", "http://127.0.0.1:8080/c/build/v1"},
	}
	for _, tc := range cases {
		out, err := runWithConfig(t, "", nil, "config", tc.agent, "--cassette", "build")
		if err != nil {
			t.Fatalf("%s: %v", tc.agent, err)
		}
		if !strings.Contains(out, tc.wantBase) {
			t.Errorf("%s: output does not carry the base URL %q:\n%s", tc.agent, tc.wantBase, out)
		}
		// A command to run, not just a setting to interpret.
		if !strings.Contains(out, tc.agent+" ") {
			t.Errorf("%s: output has no command to run:\n%s", tc.agent, out)
		}
	}
	// And the environment-only form is bare VAR=VALUE, for sourcing.
	out, err := runWithConfig(t, "", nil, "config", "claude", "--cassette", "build", "--env-only")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "ANTHROPIC_BASE_URL=http://127.0.0.1:8080/c/build" {
		t.Errorf("--env-only is not a bare assignment:\n%q", out)
	}
}

// An agent cs-vcr has no settings for is named, rather than printing something
// that looks right and is not.
func TestConfigRefusesAnUnknownAgent(t *testing.T) {
	if _, err := runWithConfig(t, "", nil, "config", "aider", "--cassette", "build"); err == nil {
		t.Fatal("an agent cs-vcr knows nothing about was accepted")
	}
}

// The cassette has to come from somewhere: printing settings that select no
// cassette would be printing a base URL that records into the session's.
func TestConfigForAnAgentNeedsACassette(t *testing.T) {
	if _, err := runWithConfig(t, "", nil, "config", "claude"); err == nil {
		t.Fatal("agent settings were printed with no cassette named")
	}
	if _, err := runWithConfig(t, "cassette: build\n", nil, "config", "claude"); err != nil {
		t.Fatalf("the configured cassette was not used: %v", err)
	}
}
