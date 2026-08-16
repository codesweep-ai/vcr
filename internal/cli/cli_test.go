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

// Clients are matched in order, so a catch-all before a prefix would make the
// prefix dead config that reads as though it works.
func TestConfigRejectsUnreachableClient(t *testing.T) {
	cfg := `clients:
  - label: everything
  - label: feature
    match: {path_prefix: /c/feature}
`
	if _, err := runWithConfig(t, cfg, nil, "config"); err == nil {
		t.Fatal("a client that can never match was accepted")
	}
}

func TestConfigRejectsClientWithoutLabel(t *testing.T) {
	cfg := "clients:\n  - match: {path_prefix: /c/feature}\n"
	if _, err := runWithConfig(t, cfg, nil, "config"); err == nil {
		t.Fatal("a client with no label was accepted")
	}
}

// The clients table is what a user checks after mistyping a base URL.
func TestConfigPrintsClients(t *testing.T) {
	cfg := `clients:
  - label: feature.default
    match: {path_prefix: /c/feature}
    cassette: refactor-auth
`
	out, err := runWithConfig(t, cfg, nil, "config")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"feature.default", "/c/feature", "refactor-auth"} {
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

// Two clients sharing a cassette share a key namespace, and a campaign's
// members are handed the same opening prompt — so their first requests
// normalize to the same bytes and one is served the other's response while
// RECORDING. The config is refused rather than diagnosed later, because by the
// time it shows up the recording is already wrong and looks complete.
func TestConfigRejectsClientsSharingACassette(t *testing.T) {
	cfg := `clients:
  - label: orchestrator
    match: {path_prefix: /c/orchestrator}
    cassette: campaign
  - label: worker
    match: {path_prefix: /c/worker}
    cassette: campaign
`
	out, err := runWithConfig(t, cfg, nil, "config")
	if err == nil {
		t.Fatal("two clients recording into one cassette were accepted")
	}
	if !strings.Contains(err.Error(), "campaign") {
		t.Errorf("the error does not name the shared cassette: %v\n%s", err, out)
	}
}

// Clients that leave `cassette:` unset all fall back to the session's, which is
// the same collision by another route.
func TestConfigRejectsClientsFallingBackToOneCassette(t *testing.T) {
	cfg := `cassette: shared
clients:
  - label: orchestrator
    match: {path_prefix: /c/orchestrator}
  - label: worker
    match: {path_prefix: /c/worker}
`
	if _, err := runWithConfig(t, cfg, nil, "config"); err == nil {
		t.Fatal("two clients defaulting to the session cassette were accepted")
	}
}
