package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codesweep-ai/vcr/internal/cassette"
	"github.com/codesweep-ai/vcr/internal/config"
)

// These tests drive the real `record`/`replay` commands over a real socket,
// because the property they protect lives in the wiring between the command and
// the server — not in either one alone.
//
// The proxy package's own tests construct the offline server themselves, so
// they pass whatever the command layer does with it. A command that built an
// online server, logged `offline=true` from its own flag and sent every miss to
// the provider would satisfy every one of them. Nothing below the command can
// observe that, so the test belongs here.

// emptyCassette creates a cassette with no steps, which is what a session that
// replays and misses everything is replaying from.
func emptyCassette(t *testing.T, dir string) {
	t.Helper()
	if _, err := cassette.Create(dir, Version, config.Default().Normalize.Version, time.Unix(0, 0).UTC()); err != nil {
		t.Fatal(err)
	}
}

// port reserves a free port and releases it. The race is theoretical and the
// alternative — plumbing the bound address back out of the command — would add
// production surface that exists only for tests.
func port(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// serveInBackground runs a serve command against providers that cannot be
// dialled, until the test ends, and returns the address to send requests to
// once /healthz answers.
func serveInBackground(t *testing.T, args ...string) string {
	return serveSessionOver(t, unreachableProviders, []string{testCassette}, args...).addr
}

// testCassette is the cassette these tests address. Its name is on the base URL
// of every request, because that is the only way a request names one.
const testCassette = "session"

// testProvider is the entry every test path routes to, and the first segment of
// every prefix a test sends.
const testProvider = "anthropic"

// session is a serve command under test, for the cases that end it themselves
// and read what it left behind.
type session struct {
	addr      string        // the proxied address
	cassettes string        // the cassette store the session was given
	out       *bytes.Buffer // where the summary is printed on the way out
	stop      func()        // end the session, and wait for it to exit
}

func serveSession(t *testing.T, cfgYAML string, args ...string) session {
	return serveSessionOver(t, cfgYAML, nil, args...)
}

// serveSessionOver is serveSession with cassettes that already exist, for the
// replay tests that reach several of them through prefixes.
func serveSessionOver(t *testing.T, cfgYAML string, existing []string, args ...string) session {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	listen, admin := port(t), port(t)

	out := &bytes.Buffer{}
	cassettes := filepath.Join(dir, "cassettes")
	// `replay` refuses a cassette that is not there, so a replay test has to
	// have one: it never recorded anything, and nothing else would have made it.
	if args[0] == "replay" {
		for _, name := range existing {
			emptyCassette(t, filepath.Join(cassettes, name))
		}
	}
	app := &App{Getenv: func(string) string { return "" }}
	cmd := newRootCmd(app)
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(append([]string{
		"--config", cfgPath,
		args[0],
		"--cassettes", cassettes,
		"--listen", listen, "--admin", admin,
	}, args[1:]...))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Error("serve did not shut down")
			}
		})
	}
	t.Cleanup(stop)

	// Ready when /healthz answers — the same signal a pipeline waits on.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + admin + "/healthz")
		if err == nil {
			resp.Body.Close()
			return session{addr: listen, cassettes: cassettes, out: out, stop: stop}
		}
		select {
		case err := <-done:
			t.Fatalf("serve exited before it was ready: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("serve never became ready")
	return session{}
}

// A provider address that cannot be dialled, so that any attempt to reach one
// is loud: a refused connection surfaces as a 502 the assertions can name.
const unreachableProviders = `providers:
  anthropic: {base_url: "http://127.0.0.1:1"}
  openai: {base_url: "http://127.0.0.1:1"}
`

// What a CI job that only ever replays actually has: no provider it could use
// if it wanted to. The two tests below run this one config through both
// commands, which is the whole of the claim.
const unusableProviders = `providers:
  anthropic: {base_url: "nonsense"}
`

// Goal 1 asks a recorded session to replay with no provider configured. A
// replay session that validated providers met that with a startup failure over
// a base_url it would never have dialled — the offline guarantee broken one
// step before the request path, where nothing was watching for it.
func TestReplayServesWithNoUsableProvider(t *testing.T) {
	s := serveSessionOver(t, unusableProviders, []string{testCassette}, "replay")

	status, body := postMessages(t, s.addr, `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hello"}]}`)

	if status != http.StatusBadRequest {
		t.Fatalf("a request with no recording = %d %s, want the 400 of a miss", status, body)
	}
}

// The counterpart, and why the check did not simply go away: `record` forwards,
// so a provider it cannot use is a 502 partway through a session that has
// already spent money. It is refused before the listener opens.
func TestRecordRefusesAProviderItCannotUse(t *testing.T) {
	out, err := runWithConfig(t, unusableProviders, nil,
		"record", "--listen", port(t), "--admin", port(t))

	if err == nil {
		t.Fatal("record started with a base_url it could not dial")
	}
	if !strings.Contains(err.Error(), "base_url") {
		t.Errorf("the error does not name the setting to fix: %v\n%s", err, out)
	}
}

func postMessages(t *testing.T, addr, body string) (int, string) {
	return postPath(t, addr, "/c/"+testProvider+"/"+testCassette+"/v1/messages", body)
}

// postPath is postMessages to a chosen path, for the cases where the path is
// the thing under test — a cassette prefix in front of the surface.
func postPath(t *testing.T, addr, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(out)
}

// The guarantee `replay --help` prints, asserted against a running server: a
// request with no recording must be reported as a miss, and must not become a
// request to the provider.
//
// A replay server built online fails this the way a broken guarantee fails in
// practice — not with a miss, but with the provider's own error handed back to
// the agent, which retries it as a transient server fault and hangs the run.
func TestReplayNeverContactsAProvider(t *testing.T) {
	addr := serveInBackground(t, "replay")

	status, body := postMessages(t, addr, `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hello"}]}`)

	if status == http.StatusBadGateway {
		t.Fatalf("replay dialled the provider: %d %s", status, body)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("a request with no recording = %d %s, want 400", status, body)
	}
	var e struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("miss body is not JSON: %v\n%s", err, body)
	}
	if e.Error.Type != "cassette_miss" {
		t.Errorf("miss reported as %q, want cassette_miss:\n%s", e.Error.Type, body)
	}
}

// The counterpart, so that the test above cannot pass by the request never
// arriving: the same request under `record` does reach for the provider, and
// fails because that provider is unreachable rather than because it was never
// tried. One address, two commands, opposite behaviour — which is the whole
// claim the two commands make.
func TestRecordDoesContactAProvider(t *testing.T) {
	addr := serveInBackground(t, "record")

	status, body := postMessages(t, addr, `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hello"}]}`)

	if status != http.StatusBadGateway {
		t.Fatalf("record did not try the provider: %d %s", status, body)
	}
	if !strings.Contains(body, "upstream") {
		t.Errorf("502 body does not name the upstream failure:\n%s", body)
	}
}

// A miss must say enough to fix it. "Cassette miss" against a cassette of
// fifteen entries, with nothing else, says only that something is wrong.
func TestReplayMissNamesTheRequest(t *testing.T) {
	addr := serveInBackground(t, "replay")

	_, body := postMessages(t, addr, `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hello"}]}`)

	for _, want := range []string{"/v1/messages", testCassette} {
		if !strings.Contains(body, want) {
			t.Errorf("miss does not mention %q:\n%s", want, body)
		}
	}
}

// /healthz is on its own listener so that a readiness probe is not a request to
// Anthropic. Asserted from the proxied port: it must not answer there.
func TestHealthzIsNotOnTheProxiedPort(t *testing.T) {
	addr := serveInBackground(t, "replay")

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("/healthz answered on the proxied port; it would be forwarded to a provider in record mode")
	}
}

// Two agents, two cassettes, one server: a campaign runs several members
// through a single cs-vcr, told apart by a prefix on the base URL. Nothing
// declares either cassette — the prefix names it, and the session opens it on
// the first request that asks for it.
func TestCassettePrefixesSeparateAgents(t *testing.T) {
	addr := serveSessionOver(t, unreachableProviders,
		[]string{testCassette, "orchestrator", "worker"}, "replay").addr

	// Each prefix is accepted and reported as a miss against its own cassette.
	for _, prefix := range []string{"/c/anthropic/orchestrator", "/c/anthropic/worker"} {
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+prefix+"/v1/messages",
			strings.NewReader(`{"model":"m","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want a 400 miss", prefix, resp.StatusCode)
		}
	}

	// A third cassette, reached the same way, with nothing declaring any of them.
	status, body := postMessages(t, addr, `{"model":"m","messages":[]}`)
	if status != http.StatusBadRequest || !strings.Contains(body, `cassette \"`+testCassette+`\"`) {
		t.Errorf("a request naming the session cassette was not served from it: %d %s", status, body)
	}

	// A prefix naming a cassette the store does not hold is refused, and named,
	// rather than being created and then missing on every request.
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/c/anthropic/never-recorded/v1/messages",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(b), "unknown_cassette") {
		t.Errorf("an unrecorded cassette was accepted: %d %s", resp.StatusCode, b)
	}
}

// A provider that is a real server, for the tests about what happens while a
// response is still arriving.
func providers(url string) string {
	return fmt.Sprintf("providers:\n  anthropic: {base_url: %q}\n  openai: {base_url: %q}\n", url, url)
}

// heldUpstream streams one event and then waits, so a test can hold a response
// open for as long as it needs to. It reports when the event has gone out.
func heldUpstream(t *testing.T) (h http.HandlerFunc, streaming <-chan struct{}, release func()) {
	t.Helper()
	began, done := make(chan struct{}), make(chan struct{})
	var beganOnce, doneOnce sync.Once
	release = func() { doneOnce.Do(func() { close(done) }) }
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: first\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		beganOnce.Do(func() { close(began) })
		select {
		case <-done:
			_, _ = io.WriteString(w, "event: last\ndata: {}\n\n")
		case <-r.Context().Done():
		}
	}, began, release
}

// waitUntilRefused blocks until the address stops accepting, which is how a
// test knows a graceful shutdown has begun: Shutdown closes the listeners
// first, then waits on what is still being answered.
func waitUntilRefused(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return
		}
		c.Close()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the proxied listener never stopped accepting")
}

// A session told to stop must not walk out on a response that is still
// arriving. The cassette entry is written once the response is done, so exiting
// underneath one throws away an interaction the provider was already paid for —
// and the trace it left was a summary reading `upstream calls 2 / recorded 1`,
// which is not a sentence anyone reads as "a turn was lost". Five seconds used
// to be the whole allowance, which is nowhere near a model composing an answer.
func TestStoppingWaitsForAResponseStillArriving(t *testing.T) {
	upstream, streaming, release := heldUpstream(t)
	up := httptest.NewServer(upstream)
	// LIFO: the held response is let go before Close waits for it.
	defer up.Close()
	defer release()

	sess := serveSession(t, providers(up.URL), "record")

	got := make(chan string, 1)
	go func() {
		resp, err := http.Post("http://"+sess.addr+"/c/"+testProvider+"/"+testCassette+"/v1/messages", "application/json",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			got <- "request failed: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		got <- string(b)
	}()

	<-streaming // the response has begun and upstream is holding it open
	stopped := make(chan struct{})
	go func() { sess.stop(); close(stopped) }()
	waitUntilRefused(t, sess.addr) // the shutdown is under way
	release()                      // and only now does the answer finish

	select {
	case body := <-got:
		if !strings.Contains(body, "event: last") {
			t.Errorf("the client did not receive the rest of its response:\n%q", body)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the response never completed")
	}
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("the session never exited")
	}

	summary := sess.out.String()
	if !regexp.MustCompile(`(?m)^recorded\s+1\b`).MatchString(summary) {
		t.Errorf("the interaction was not recorded:\n%s", summary)
	}
	if strings.Contains(summary, "abandoned") {
		t.Errorf("a response that finished was reported as abandoned:\n%s", summary)
	}
}

// The length of that wait is the substance of it, and no test that finishes in
// a second can observe it: what it has to outlast is a model composing an
// answer, not a socket closing. So the number is asserted directly, with its
// reason, and going back to something sized for an idle server has to be
// deliberate.
func TestTheDrainOutlastsAModelAnswering(t *testing.T) {
	if drainTimeout < time.Minute {
		t.Errorf("drainTimeout = %s: a streamed answer routinely runs longer than that, "+
			"and a session that stops underneath one throws the recording away", drainTimeout)
	}
}

// The other half: the wait is bounded, and when it runs out the session says
// what it walked out on rather than printing numbers that quietly do not add
// up. This is the only place a lost recording is visible.
func TestARecordingTheSessionWalkedOutOnIsReported(t *testing.T) {
	// Shortened so the test does not spend the real allowance waiting for an
	// upstream that has been told never to finish.
	defer func(d time.Duration) { drainTimeout = d }(drainTimeout)
	drainTimeout = 200 * time.Millisecond

	upstream, streaming, release := heldUpstream(t)
	up := httptest.NewServer(upstream)
	defer up.Close()
	defer release()

	sess := serveSession(t, providers(up.URL), "record")
	go func() {
		resp, err := http.Post("http://"+sess.addr+"/c/"+testProvider+"/"+testCassette+"/v1/messages", "application/json",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	<-streaming
	sess.stop()

	summary := sess.out.String()
	if !regexp.MustCompile(`(?m)^abandoned\s+1\b`).MatchString(summary) {
		t.Errorf("a recording the session gave up on is not reported:\n%s", summary)
	}

	// The session is gone but this process is not, so the request it walked out
	// on is still running and still writing. Let it land before the test's
	// directory is taken out from under it.
	release()
	waitForFile(t, filepath.Join(sess.cassettes, testCassette, "index.jsonl"))
}

// waitForFile blocks until a path exists, for the writes that outlive what
// started them.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("%s never appeared", path)
}

// The summary a run is read by. A pipeline decides on the exit status, but a
// human decides on this, and a replay session that quietly recorded nothing
// looks identical to a successful one without it.
func TestReplaySummaryReportsMisses(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(unreachableProviders), 0o644); err != nil {
		t.Fatal(err)
	}
	listen, admin := port(t), port(t)

	app := &App{Getenv: func(string) string { return "" }}
	cmd := newRootCmd(app)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	emptyCassette(t, filepath.Join(dir, "cassettes", testCassette))
	cmd.SetArgs([]string{"--config", cfgPath, "replay",
		"--cassettes", filepath.Join(dir, "cassettes"),
		"--listen", listen, "--admin", admin})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get("http://" + admin + "/healthz"); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	postMessages(t, listen, `{"model":"m","messages":[]}`)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not shut down")
	}

	if got := out.String(); !strings.Contains(got, strconv.Itoa(1)) || !strings.Contains(strings.ToLower(got), "miss") {
		t.Errorf("the summary does not report the miss:\n%s", got)
	}
}

// A prefix may name a cassette that does not exist yet, and `record` creates
// it. That is what lets a build record a new scenario without anything having
// declared it, which is the whole of the configuration this replaced.
func TestRecordCreatesACassetteAPrefixNames(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()

	sess := serveSessionOver(t, providers(up.URL), nil, "record")
	if status, body := postPath(t, sess.addr, "/c/anthropic/on-demand/v1/messages",
		`{"model":"m","messages":[]}`); status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}
	sess.stop()

	index := filepath.Join(sess.cassettes, "on-demand", "index.jsonl")
	b, err := os.ReadFile(index)
	if err != nil {
		t.Fatalf("the named cassette was not written: %v", err)
	}
	if n := len(strings.Split(strings.TrimSpace(string(b)), "\n")); n != 1 {
		t.Errorf("index has %d steps, want 1:\n%s", n, b)
	}
	// And the summary attributes it by cassette, which is what a reader of a CI
	// log uses to tell one scenario's traffic from another's.
	if got := sess.out.String(); !strings.Contains(got, "cassette on-demand") {
		t.Errorf("the summary does not attribute the traffic:\n%s", got)
	}
}

// Replay refuses a cassette that is not in the store, and does not reach a
// provider to find out. Creating one instead would answer every request with a
// miss, which reads as an agent that diverged rather than as a name that was
// wrong.
func TestReplayRefusesACassetteThatWasNeverRecorded(t *testing.T) {
	addr := serveSession(t, unreachableProviders, "replay").addr
	status, body := postPath(t, addr, "/c/anthropic/never-recorded/v1/messages", `{"model":"m","messages":[]}`)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — a 502 would mean it tried the provider", status)
	}
	if !strings.Contains(body, "unknown_cassette") || !strings.Contains(body, "never-recorded") {
		t.Errorf("the error does not name what was missing: %s", body)
	}
}

// A cassette from another build is not a cassette that is missing, and saying
// so sends the reader to look for a directory that is sitting right there.
func TestReplayNamesACassetteItCannotRead(t *testing.T) {
	sess := serveSessionOver(t, unreachableProviders, []string{"stale"}, "replay")

	// Age it under the running session, which is when a prefix first reaches
	// for it: the store is consulted on the request, not at startup.
	meta := filepath.Join(sess.cassettes, "stale", "cassette.yaml")
	b, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	old := regexp.MustCompile(`normalize_version: \d+`).ReplaceAll(b, []byte("normalize_version: 1"))
	if err := os.WriteFile(meta, old, 0o644); err != nil {
		t.Fatal(err)
	}

	status, body := postPath(t, sess.addr, "/c/anthropic/stale/v1/messages", `{"model":"m","messages":[]}`)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — a stale cassette must refuse the request, not invite a retry", status)
	}
	if !strings.Contains(body, "cassette_unusable") || !strings.Contains(body, "stale") {
		t.Errorf("the error does not say what is wrong: %s", body)
	}
}
