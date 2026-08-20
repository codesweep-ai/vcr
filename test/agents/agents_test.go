package agents

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The two halves of the suite are separate tests behind separate switches, so
// that `go test ./...` runs neither. One of them spends money and the other
// needs three agents installed; both are make targets rather than something a
// contributor trips over.
const (
	recordSwitch = "CS_VCR_AGENTS_RECORD"
	replaySwitch = "CS_VCR_AGENTS"
	// strictSwitch turns every skip into a failure. CI sets it: a job that
	// silently skipped its whole matrix reports the same green as one that ran
	// it, and the first is the one that lets a broken fixture through.
	strictSwitch = "CS_VCR_AGENTS_STRICT"
)

// The variables a recorded cassette is scrubbed against. Naming them is what
// makes the scrub exact rather than shape-based: whatever is in these must not
// appear in a committed fixture, whatever it looks like.
//
// HOME is here with the keys because a fixture is published and a home
// directory names a person. Normalization reaches the workspace under it in
// every form a tool prints; this catches a path outside the workspace that some
// tool decided to mention.
var secretVars = []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "FIREWORKS_API_KEY", "HOME"}

// TestRecordFixtures records one real session per scenario, scrubs it, and then
// replays it before keeping it.
//
// The replay is part of recording rather than a separate step, because a fixture
// that does not replay is not a fixture. It is also what proves the scrub was
// safe: a value taken out of a request changes what replay matches on, and this
// is where that shows up — on the machine that can re-record, not in CI.
func TestRecordFixtures(t *testing.T) {
	if os.Getenv(recordSwitch) == "" {
		t.Skipf("set %s=1 to record fixtures (this calls real providers and spends money) — `make fixtures`", recordSwitch)
	}
	cassettes := cassetteDir(t)
	man, err := loadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	man.Prompt = prompt
	// An entry no scenario claims is dead: a scenario that was renamed or
	// dropped leaves one behind, and it would go on pinning an agent version CI
	// installs for a cassette nothing replays.
	for _, name := range man.names() {
		if !claimed(name) {
			delete(man.Fixtures, name)
			t.Logf("dropped %s from the manifest: no scenario claims it", name)
		}
	}

	for _, sc := range scenarios() {
		t.Run(sc.name, func(t *testing.T) {
			version := requireAgent(t, sc)
			cred, err := sc.needs(os.Getenv)
			if err != nil {
				skip(t, "%s cannot be recorded here: %v", sc.name, err)
			}
			// Re-recording replaces the session rather than appending to it: a
			// cassette holds ONE session, and a second recording into the same
			// directory leaves a script holding both.
			dir := filepath.Join(cassettes, sc.name)
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}

			steps := runScenario(t, sc, cred, record, cassettes)
			if steps == 0 {
				t.Fatalf("%s recorded no steps", sc.name)
			}

			// Nothing sensitive leaves this machine. The named variables catch
			// the exact values this host authenticated with; the built-in shapes
			// catch a credential the session quoted from somewhere else.
			out, err := runCassetteCmd(cassettes, "cassette", "scrub", sc.name,
				"--force", "--from-env", strings.Join(secretVars, ","))
			if err != nil {
				t.Fatalf("scrubbing %s: %v\n%s", sc.name, err, out)
			}
			t.Log(strings.TrimSpace(out))

			// And it replays, here, before it is committed.
			replayed := runScenario(t, sc, cred, replay, cassettes)
			if replayed != steps {
				t.Fatalf("%s recorded %d steps and replayed %d", sc.name, steps, replayed)
			}

			man.Fixtures[sc.name] = fixture{
				Agent: sc.bin, Version: version, Auth: sc.auth, Model: sc.model,
				Steps: steps, Recorded: time.Now().UTC().Format("2006-01-02"),
			}
			if err := man.save(manifestPath); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestReplayFixtures is the half CI runs: every committed cassette, replayed by
// the agent that recorded it, with fabricated credentials and no provider
// reachable.
func TestReplayFixtures(t *testing.T) {
	if os.Getenv(replaySwitch) == "" {
		t.Skipf("set %s=1 to replay the committed fixtures — `make test-integration`", replaySwitch)
	}
	cassettes := cassetteDir(t)
	man, err := loadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Fixtures) == 0 {
		t.Skip("no fixtures are committed yet — record them with `make fixtures`")
	}
	if man.Prompt != prompt {
		t.Fatalf("the fixtures were recorded with a different prompt:\n  recorded: %q\n  this run: %q\n"+
			"re-record with `make fixtures`", man.Prompt, prompt)
	}

	for _, sc := range scenarios() {
		fx, ok := man.Fixtures[sc.name]
		if !ok {
			// Never a failure, even under the strict switch: a scenario with no
			// fixture is a combination nobody has been able to sign in for, and
			// CI cannot record one either. What strictness is for is a host that
			// was supposed to be able to replay and could not.
			t.Run(sc.name, func(t *testing.T) {
				t.Skipf("no committed fixture for %s — record one with `make fixtures`", sc.name)
			})
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			// The ruleset the fixture was recorded under, checked before
			// anything is started. A cassette from an older ruleset is refused
			// per request rather than at startup, so without this the run
			// reaches the proxy, is refused, and reports whatever the client
			// makes of a refusal — and the explanation lives in a log this test
			// prints only when something else fails.
			//
			// A failure rather than a skip, on every host: an agent that is not
			// installed is a gap in what this machine can cover, but a fixture
			// recorded under a ruleset this build no longer speaks is committed
			// and wrong, and is wrong for everybody.
			if out, err := runCassetteCmd(cassettes, "cassette", "verify", sc.name); err != nil {
				t.Fatalf("%s cannot be replayed by this build:\n%s\nre-record with `make fixtures`",
					sc.name, strings.TrimSpace(out))
			}
			version := requireAgent(t, sc)
			// The agent's own version is in its prompt, so a different build
			// sends a different request. Said plainly here, because the
			// alternative is a hundred lines of prompt diff that never mentions
			// the cause.
			if version != fx.Version {
				skip(t, "%s was recorded with %s %s and this host has %s — install %s@%s, or re-record with `make fixtures`",
					sc.name, sc.bin, fx.Version, version, sc.bin, fx.Version)
			}
			replayed := runScenario(t, sc, fabricated(sc), replay, cassettes)
			// Fewer steps than were recorded is not a failure, and cannot be
			// made into one: a client decides for itself how many times to ask
			// for the model list at startup, and Codex asks twice on some runs
			// and once on others. What must hold is that nothing missed and
			// nothing was fetched, and that the agent did the work — all
			// asserted in runScenario, and none of it possible on a script that
			// stopped early.
			if replayed < fx.Steps {
				t.Logf("%s served %d of the %d steps recorded; the rest were startup probes this run did not repeat",
					sc.name, replayed, fx.Steps)
			}
		})
	}
}

// runScenario is one whole run: a clean workspace, a cs-vcr session, the agent,
// and the assertions that both did what they were supposed to. It returns the
// number of steps the session recorded or replayed.
func runScenario(t *testing.T, sc scenario, cred credential, m mode, cassettes string) int {
	t.Helper()
	ws, err := newWorkspace(sc.name, m.String())
	if err != nil {
		t.Fatal(err)
	}
	// The recording half gets a config file of its own, written per run: it has
	// to reach this scenario's provider, and what a developer keeps in
	// ~/.config/cs-vcr/config.yaml must not be what decides where the traffic
	// went.
	//
	// The replay half gets none, and is given no --config either. A scenario's
	// settings are all provider settings — where its upstream lives, and where
	// a path cs-vcr does not model goes — and replay reads none of them, so it
	// runs on whatever the machine already has. That is Goal 1 asserted against
	// a real agent rather than assumed.
	configPath := ""
	if m == record {
		configPath = filepath.Join(ws.root, "cs-vcr.yaml")
		if err := os.WriteFile(configPath, []byte(sc.vcrConfig), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	missDir := ""
	if m == replay {
		missDir = filepath.Join(ws.root, "misses")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	p, err := startProxy(ctx, ws, cassettes, configPath, m == replay, missDir)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = p.stop()
		}
	}()

	base := p.baseURL(sc.name, sc.urlSuffix)
	if err := sc.prepare(sc, ws, cred, m, base); err != nil {
		t.Fatal(err)
	}
	cmd := sc.command(sc, ws, cred, m, base)
	out, agentErr := combined(ctx, cmd)

	stopped = true
	proxyErr := p.stop()

	// The agent's own result first: a run that failed for its own reasons
	// explains a cassette that came out short, and the proxy summary below is
	// the evidence for it.
	if agentErr != nil {
		t.Fatalf("%s (%s) failed: %v\n--- agent output ---\n%s\n--- cs-vcr ---\n%s",
			sc.name, m, agentErr, tail(out, 40), tail(p.log(), 40))
	}
	s := p.summary

	// What the agent said and what it did, in that order: an agent that
	// answered without acting and one that could not run its tools at all fail
	// differently, and the transcript is the only place either says why.
	if !strings.Contains(strings.ToLower(out), doneAnswer) {
		t.Errorf("%s (%s): the agent never said %q\n%s", sc.name, m, doneAnswer, tail(out, 20))
	}
	if err := ws.wroteHello(); err != nil {
		t.Errorf("%s (%s): %v%s\n--- agent output ---\n%s", sc.name, m, err, hostHint(m, s), tail(out, 30))
	}
	if m == replay {
		// The property the whole project rests on, asserted rather than
		// assumed: a replay session reached no provider, and served every
		// request from the cassette.
		if proxyErr != nil {
			t.Fatalf("%s: replay failed: %v\n%s\n%s", sc.name, proxyErr, tail(p.log(), 60), missReport(missDir))
		}
		if s.Upstream != 0 {
			t.Errorf("%s: replay made %d upstream call(s)", sc.name, s.Upstream)
		}
		if s.Misses != 0 {
			t.Errorf("%s: replay missed %d request(s)\n%s", sc.name, s.Misses, tail(p.log(), 40))
		}
		return s.Replayed
	}
	if proxyErr != nil {
		t.Fatalf("%s: recording failed: %v\n%s", sc.name, proxyErr, tail(p.log(), 40))
	}
	if s.Recorded != s.Requests {
		t.Errorf("%s: %d of %d requests were recorded\n%s", sc.name, s.Recorded, s.Requests, tail(p.log(), 40))
	}
	return s.Recorded
}

// hostHint separates a cassette that stopped matching from a host that cannot
// run the agent's tools.
//
// A replay that served every step and reached no provider did its job; if the
// work still did not happen, what failed is the agent's own sandbox. Codex asks
// bubblewrap for an unprivileged user namespace, and Ubuntu 24.04 denies one to
// a binary with no AppArmor profile — including the copy Codex bundles. Without
// this line the failure reads as a broken fixture, and sends whoever hits it to
// re-record one that was never wrong.
func hostHint(m mode, s summary) string {
	if m != replay || s.Misses != 0 || s.Upstream != 0 || s.Replayed == 0 {
		return ""
	}
	return fmt.Sprintf("\nthe cassette is not the problem: %d step(s) replayed, %d misses, %d upstream calls. "+
		"The agent could not run its own tool — on Linux that is usually a sandbox denied an unprivileged "+
		"user namespace (see INSTALL.md).", s.Replayed, s.Misses, s.Upstream)
}

// combined runs the agent and returns everything it printed. Its output is the
// only place some failures appear — an agent that cannot sign in says so on
// stdout and exits 0.
func combined(ctx context.Context, cmd *exec.Cmd) (string, error) {
	b, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return string(b), fmt.Errorf("timed out: %w", ctx.Err())
	}
	return string(b), err
}

// requireAgent skips (or, in CI, fails) when the agent is not installed, and
// returns the version it found.
func requireAgent(t *testing.T, sc scenario) string {
	t.Helper()
	if _, err := exec.LookPath(sc.bin); err != nil {
		skip(t, "%s is not installed, so %s cannot run", sc.bin, sc.name)
	}
	v, err := agentVersion(sc.bin)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// skip reports why a scenario did not run. Under the strict switch it fails
// instead: CI has to be told what it could not do, or a matrix that skipped
// itself reads as a matrix that passed.
func skip(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv(strictSwitch) != "" {
		t.Fatalf("%s is set and this scenario skipped: %s", strictSwitch, fmt.Sprintf(format, args...))
	}
	t.Skipf(format, args...)
}

// claimed reports whether a manifest entry still belongs to a scenario.
func claimed(name string) bool {
	for _, sc := range scenarios() {
		if sc.name == name {
			return true
		}
	}
	return false
}

// fabricated is the credential the replay half signs in with: the variables
// this scenario expects, and nothing that could authenticate.
func fabricated(sc scenario) credential {
	c := credential{env: map[string]string{}}
	if sc.keyAs != "" {
		c.env[sc.keyAs] = ""
	}
	return c
}

// cassetteDir is the committed store at the repository root. The suite runs in
// its own package directory, and the fixtures belong beside the code they test.
func cassetteDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "cassettes"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// missReport names the requests a failed replay could not serve, which is what
// `calibrate` is then run against.
func missReport(dir string) string {
	if dir == "" {
		return ""
	}
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) == 0 {
		return ""
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return fmt.Sprintf("missed requests written to %s: %s\n"+
		"diff them against the cassette, or run `cs-vcr calibrate <cassette> %s`",
		dir, strings.Join(names, " "), dir)
}

// tail keeps the last n lines, because an agent's transcript is long and the end
// of it is where it said what went wrong.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return "…\n" + strings.Join(lines[len(lines)-n:], "\n")
}
