package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/codesweep-ai/vcr/internal/cassette"
	"github.com/codesweep-ai/vcr/internal/config"
)

// A credential a real agent would be holding — an API key, or the OAuth token
// a Claude Pro/Max login leaves behind. cs-vcr treats both as opaque: it does
// not read, validate, store or replace either.
const clientCred = "sk-ant-oat01-CLIENT-CREDENTIAL"

// What the one asserted bit means at a call site. There is no mode: naming a
// cassette is what asks for recording, and this says whether a request with no
// recording may reach a provider.
const (
	online  = false
	offline = true
)

// logBuffer is the log sink these tests read back. Guarded, because a test that
// drives the server over a real socket has net/http's goroutine writing to it
// while the test goroutine reads what it wrote.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// newTestServer builds a proxy against a local upstream. tune adjusts the
// configuration before it is resolved, which is how a test gives a provider an
// upstream of its own; nil is the plain deployment.
func newTestServer(t *testing.T, offline bool, tune func(*config.Config), upstream http.HandlerFunc) (*Server, *logBuffer) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)

	cfg := config.Default()
	for name := range cfg.Providers {
		cfg.Providers[name] = &config.Provider{BaseURL: up.URL}
	}
	if tune != nil {
		tune(cfg)
	}
	if err := cfg.Resolve(); err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	srv := New(cfg, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), offline)
	return srv, logs
}

// testCassette is the cassette the helpers below address. Every request names
// one now, so a test about a body, a stream or a header would otherwise carry
// the prefix ninety times over while saying nothing about it. The tests that
// ARE about naming write their own paths, and onCassette leaves those alone.
const testCassette = "session"

// testProvider is the entry every test path routes to. Both providers in the
// default configuration point at the same upstream, so the segment is about
// what the URL says rather than about where the bytes go.
const testProvider = "anthropic"

func onCassette(path string) string {
	if strings.HasPrefix(path, config.Prefix) {
		return path
	}
	return config.Prefix + testProvider + "/" + testCassette + path
}

func post(t *testing.T, s *Server, path string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, onCassette(path), strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// get is for the bodiless requests an agent makes around its prompts — the
// model list, a startup probe — which a session needs replayed just as much.
func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, onCassette(path), http.NoBody))
	return w
}

// The core contract: what the client sent is what upstream gets. cs-vcr is a
// recorder, so anything it changed on the way through would be a difference
// between the recording and the interaction it claims to have recorded.
func TestHeadersCrossUnchanged(t *testing.T) {
	var got http.Header
	s, _ := newTestServer(t, online, nil, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	sent := map[string]string{
		"authorization":     "Bearer " + clientCred,
		"anthropic-version": "2023-06-01",
		"anthropic-beta":    "claude-code-20250219,interleaved-thinking-2025-05-14",
		"x-app":             "cli",
	}
	w := post(t, s, "/v1/messages", sent, `{"model":"claude-sonnet-5"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	for k, v := range sent {
		if got.Get(k) != v {
			t.Errorf("upstream %s = %q, want %q unchanged", k, got.Get(k), v)
		}
	}
}

// A request with no credential at all is forwarded too, and upstream decides.
// cs-vcr has no opinion about whether a caller is authenticated: inventing one
// would mean maintaining a second, worse copy of each provider's auth rules.
func TestRequestWithNoCredentialIsStillForwarded(t *testing.T) {
	reached := false
	s, _ := newTestServer(t, online, nil, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusUnauthorized)
	})
	w := post(t, s, "/v1/messages", nil, `{}`)
	if !reached {
		t.Fatal("cs-vcr rejected a request instead of letting upstream answer")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want upstream's own 401 passed back", w.Code)
	}
}

// Replay mode never reaches upstream, whatever else is configured.
// Asserted with an upstream that fails the test if it is dialled at all —
// forwarding a miss upstream is how a $0 bill becomes a surprise invoice.
func TestReplayModeNeverContactsUpstream(t *testing.T) {
	s, _ := newTestServer(t, offline, nil, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay mode contacted the provider")
	})
	w := post(t, s, "/v1/messages", map[string]string{"authorization": "Bearer " + clientCred}, `{}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("miss response is not the structured error clients parse: %v", err)
	}
	if body.Error.Type != "cassette_miss" {
		t.Errorf("error type = %q, want cassette_miss", body.Error.Type)
	}
}

// The safety bit has to agree with the behaviour it describes.
//
// ReachesUpstream is what a reader consults to decide whether a pipeline can
// spend money, and SPEC 11.3 makes it the one asserted bit. The test above
// covers the behaviour on its own, and this pairs the two: a bit that reports
// "offline" while the code around it dials is worse than no bit at all, and one
// that reports "online" while nothing dials hides a recording session that
// recorded nothing behind a green run.
//
// Both halves, because only the negative one is about money and only the
// positive one would catch the flag being inverted.
func TestReachesUpstreamMatchesWhatTheServerDoes(t *testing.T) {
	cases := []struct {
		name     string
		offline  bool
		reaches  bool
		wantCode int
	}{
		{"record", online, true, http.StatusOK},
		{"replay", offline, false, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dialled := false
			s, _ := newTestServer(t, tc.offline, nil, func(http.ResponseWriter, *http.Request) {
				dialled = true
			})
			if got := s.ReachesUpstream(); got != tc.reaches {
				t.Errorf("ReachesUpstream() = %v, want %v", got, tc.reaches)
			}
			if w := post(t, s, "/v1/messages", nil, `{}`); w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantCode)
			}
			if dialled != tc.reaches {
				t.Errorf("the provider was dialled = %v, while ReachesUpstream() promised %v",
					dialled, tc.reaches)
			}
		})
	}
}

// routed by path, so the same surface is recognized whichever agent
// produced it — OpenCode pointed at Anthropic uses the Claude Code surface, and
// "which agent is this" has no answer.
func TestRoutingBySurface(t *testing.T) {
	cases := []struct {
		path    string
		hdr     map[string]string
		surface Surface
	}{
		{"/v1/messages", map[string]string{"x-api-key": "k"}, SurfaceAnthropicMessages},
		{"/v1/messages", map[string]string{"authorization": "Bearer k"}, SurfaceAnthropicMessages},
		{"/v1/responses", map[string]string{"authorization": "Bearer k"}, SurfaceOpenAIResponses},
		{"/v1/chat/completions", map[string]string{"authorization": "Bearer k"}, SurfaceOpenAIChat},
		{"/v1/something/new", map[string]string{"authorization": "Bearer k"}, SurfaceUnknown},
		// The same surfaces unversioned, which is how they arrive when the
		// provider's own path has no /v1: Codex signed in with ChatGPT is
		// pointed at chatgpt.com/backend-api/codex and asks for /responses.
		{"/responses", map[string]string{"authorization": "Bearer k"}, SurfaceOpenAIResponses},
		{"/messages", map[string]string{"authorization": "Bearer k"}, SurfaceAnthropicMessages},
		{"/chat/completions", map[string]string{"authorization": "Bearer k"}, SurfaceOpenAIChat},
		// Only /v1 is optional, not any prefix: a version cs-vcr does not model
		// is a surface it does not know, and reading it as v1 would key a
		// request against a shape nobody checked it against.
		{"/v2/responses", map[string]string{"authorization": "Bearer k"}, SurfaceUnknown},
		{"/v1beta/responses", map[string]string{"authorization": "Bearer k"}, SurfaceUnknown},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, c.path, http.NoBody)
		for k, v := range c.hdr {
			r.Header.Set(k, v)
		}
		if got := surfaceOf(r); got != c.surface {
			t.Errorf("surfaceOf(%s) = %s, want %s", c.path, got, c.surface)
		}
	}
}

// The provider is the one the base URL named, for every path on the prefix and
// whatever the request carries.
//
// Two halves, and each has to hold on its own. A path cs-vcr does not model has
// no shape to route by: Claude Code's startup probe arrives as the runtime's
// own fetch with nothing Anthropic about it. And a header cannot stand in for
// one, because a Pro/Max subscription login sends `Authorization: Bearer`
// exactly like an OpenAI client. The prefix answers both, because a client is
// configured with one base URL per provider.
func TestTheBaseURLNamesTheProvider(t *testing.T) {
	reached := make(chan string, 8)
	upstream := func(name string) *httptest.Server {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached <- name
			_, _ = io.WriteString(w, `{}`)
		}))
		t.Cleanup(up.Close)
		return up
	}
	anthropic, openai := upstream("anthropic"), upstream("openai")
	s, _ := newTestServer(t, online, func(c *config.Config) {
		c.Providers["anthropic"] = &config.Provider{BaseURL: anthropic.URL}
		c.Providers["openai"] = &config.Provider{BaseURL: openai.URL}
	}, func(w http.ResponseWriter, _ *http.Request) {})

	cases := []struct {
		path, want string
		hdr        map[string]string
	}{
		// A modelled path, on each prefix.
		{"/c/anthropic/session/v1/messages", "anthropic", nil},
		{"/c/openai/session/v1/responses", "openai", nil},
		// A path with no shape to route by, on each prefix. This is the probe.
		{"/c/anthropic/session/api/hello", "anthropic", map[string]string{"user-agent": "Bun/1.4.0"}},
		{"/c/openai/session/models", "openai", map[string]string{"user-agent": "Bun/1.4.0"}},
		// Provider-specific headers sent against the other prefix: the URL
		// wins, so a client cannot steer its own traffic to a second upstream
		// by what it sends.
		{"/c/openai/session/api/hello", "openai", map[string]string{"anthropic-version": "2023-06-01"}},
		{"/c/openai/session/v1/messages", "openai", map[string]string{"x-api-key": "sk-ant"}},
		{"/c/anthropic/session/v1/responses", "anthropic", map[string]string{"authorization": "Bearer sk-oai"}},
	}
	for _, c := range cases {
		if w := post(t, s, c.path, c.hdr, `{}`); w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (%s)", c.path, w.Code, w.Body)
		}
		if got := <-reached; got != c.want {
			t.Errorf("%s reached %s, want %s", c.path, got, c.want)
		}
	}
}

// A provider the base URL names and this session does not have is a base URL
// typed wrongly, so it is refused with the spelling that would work — and with
// a status no SDK retries, because no retry adds a provider entry.
func TestAProviderTheConfigurationLacksIsRefused(t *testing.T) {
	reached := false
	s, logs := forwardServer(t, func(w http.ResponseWriter, _ *http.Request) { reached = true })

	w := post(t, s, "/c/anthropick/session/v1/messages", nil, `{}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (%s)", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "unknown_provider") {
		t.Errorf("error type is not unknown_provider: %s", w.Body)
	}
	// The reply names what this session does have, because the reader of it is
	// the person who wrote the base URL.
	if !strings.Contains(w.Body.String(), "anthropic") {
		t.Errorf("the error does not name a configured provider: %s", w.Body)
	}
	if reached {
		t.Error("a request naming an unconfigured provider reached upstream")
	}
	if !strings.Contains(logs.String(), "named a provider this session does not have") {
		t.Errorf("not logged: %s", logs)
	}
}

// an unrecognized path is still proxied, and the fact is logged at
// WARN. Measured against the real client: Claude Code probes /api/hello at
// startup, which is exactly this case.
func TestUnknownPathIsProxiedAndWarned(t *testing.T) {
	reached := false
	s, logs := newTestServer(t, online, nil, func(w http.ResponseWriter, r *http.Request) {
		reached = true
	})
	w := post(t, s, "/api/hello", map[string]string{"authorization": "Bearer " + clientCred}, `{}`)
	if w.Code != http.StatusOK || !reached {
		t.Fatalf("status = %d reached = %v, want the request passed through", w.Code, reached)
	}
	if !strings.Contains(logs.String(), "unrecognized path") {
		t.Errorf("unrecognized path was not noted: %s", logs)
	}
}

// The copy a recording keeps is bounded, and the response the client gets is
// not.
//
// A stream runs for as long as the model keeps talking, and nothing in HTTP
// obliges it to end. Without a bound, one long turn grows the recorder's buffer
// for the life of the request and the session dies to the out-of-memory killer
// mid-recording — a failure that reports nothing and loses everything already
// captured. With one, the cassette is short by a stated number of bytes and the
// run says so.
func TestTheRecordingCopyIsBoundedAndTheClientStillGetsEverything(t *testing.T) {
	restore := maxBody
	maxBody = 64
	t.Cleanup(func() { maxBody = restore })

	const answer = "0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF" +
		"THIS-TAIL-IS-PAST-THE-LIMIT"
	dir := filepath.Join(t.TempDir(), "oversized")
	rec, logs := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answer)
	})
	live := post(t, rec, "/v1/messages", nil, `{"model":"claude-sonnet-5"}`)

	// The client is served in full. A recorder that truncated the traffic it
	// was observing would change the session it exists to reproduce.
	if got := live.Body.String(); got != answer {
		t.Errorf("the client received %d bytes, want the whole %d-byte response", len(got), len(answer))
	}
	// The cassette holds the cap and no more.
	body, err := os.ReadFile(filepath.Join(dir, "resp", "0001.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != maxBody {
		t.Errorf("the cassette kept %d bytes, want the %d-byte cap", len(body), maxBody)
	}
	if strings.Contains(string(body), "PAST-THE-LIMIT") {
		t.Error("the cassette kept bytes past the cap")
	}
	// Loudly, because half an answer replays as half an answer.
	if !strings.Contains(logs.String(), "outgrew the capture limit") {
		t.Errorf("a truncated recording was not reported:\n%s", logs)
	}
}

// The bound costs an ordinary response nothing: it is not a chunking rule, and
// a recording that fits keeps every byte and reports nothing.
func TestAResponseUnderTheLimitIsRecordedWhole(t *testing.T) {
	restore := maxBody
	maxBody = 64
	t.Cleanup(func() { maxBody = restore })

	const answer = `{"ok":true}`
	dir := filepath.Join(t.TempDir(), "ordinary")
	rec, logs := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answer)
	})
	post(t, rec, "/v1/messages", nil, `{"model":"claude-sonnet-5"}`)

	body, err := os.ReadFile(filepath.Join(dir, "resp", "0001.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != answer {
		t.Errorf("recorded %q, want the response whole", body)
	}
	if strings.Contains(logs.String(), "outgrew the capture limit") {
		t.Errorf("a response that fit was reported as truncated:\n%s", logs)
	}
}

// sessionStore is the store a test attached with WithCassette, which is what
// the recording assertions read back.
func sessionStore(t *testing.T, s *Server) *cassette.Store {
	t.Helper()
	store, err := s.storeFor(testCassette)
	if err != nil || store == nil {
		t.Fatalf("no session store: %v", err)
	}
	return store
}

// A tolerated difference that flips a tool result from success to failure is
// counted apart from the rest.
//
// It is still tolerated: refusing it would fail every replay where a recorded
// command names the recording's own commit, which is ordinary. But it is the
// one tolerance that can cost a session its outcome — the client is handed the
// answer to a command that did not succeed here — and among a hundred ordinary
// drifts nobody would find it. Measured on a cs-campaign replay, where exactly
// this hid the reason a campaign ended with its verdict unsent.
func TestAToleratedFailureIsCountedApartFromOrdinaryDrift(t *testing.T) {
	for _, tc := range []struct {
		name     string
		d        cassette.Difference
		wantFlag bool
	}{
		{"success became failure", cassette.Difference{Path: "messages[41].content[0].is_error", Recorded: false, Live: true}, true},
		{"failure became success", cassette.Difference{Path: "messages[41].content[0].is_error", Recorded: true, Live: false}, false},
		{"unchanged flag", cassette.Difference{Path: "messages[41].content[0].is_error", Recorded: false, Live: false}, false},
		{"an ordinary output drift", cassette.Difference{Path: "messages[41].content[0].content", Recorded: "a", Live: "b"}, false},
		{"a path that merely ends in prose", cassette.Difference{Path: "messages[0].text", Recorded: "x", Live: "y"}, false},
		{"non-boolean values", cassette.Difference{Path: "x.is_error", Recorded: "false", Live: "true"}, false},
	} {
		if got := failedLive(tc.d); got != tc.wantFlag {
			t.Errorf("%s: failedLive = %v, want %v", tc.name, got, tc.wantFlag)
		}
	}
}
