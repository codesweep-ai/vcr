package proxy

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codesweep-ai/vcr/internal/cassette"
	"github.com/codesweep-ai/vcr/internal/config"
)

// A real Anthropic streamed response, trimmed. The shape matters: usage arrives
// split across message_start and message_delta, and a client assembles the text
// from deltas as they land.
const sseResponse = `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","model":"claude-sonnet-5","usage":{"input_tokens":1240,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`

// cassetteServer builds a proxy with a cassette in dir, against the given upstream.
func cassetteServer(t *testing.T, dir string, offline bool, upstream http.HandlerFunc) (*Server, *logBuffer) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)

	cfg := config.Default()
	// Both, or an unrecognized path routes to the real api.openai.com.
	cfg.Providers["anthropic"] = &config.Provider{BaseURL: up.URL}
	cfg.Providers["openai"] = &config.Provider{BaseURL: up.URL}
	if err := cfg.Resolve(); err != nil {
		t.Fatal(err)
	}
	store, err := cassette.OpenStore(dir, "test", cfg.Normalize.Version, func() int64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	s := New(cfg, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), offline).WithCassette(store)
	s.now = func() time.Time { return time.Unix(0, 0).UTC() }
	return s, logs
}

// THE test for this project: record a session locally, then replay it with the
// provider gone. If this passes, a CI job can run the whole agent loop without
// contacting anyone.
func TestRecordThenReplayWithNoProviderReachable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "refactor-auth")
	const reqBody = `{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	// --- Local: record.
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseResponse)
	})
	live := post(t, rec, "/v1/messages", map[string]string{"authorization": "Bearer whatever"}, reqBody)
	if live.Code != http.StatusOK {
		t.Fatalf("record: status = %d", live.Code)
	}
	if n := rec.Snapshot().Recorded; n != 1 {
		t.Fatalf("recorded = %d, want 1", n)
	}

	// --- CI: replay. The upstream fails the test if it is dialled at all,
	// which is the only way to be sure no provider was contacted.
	rep, logs := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	replayed := post(t, rep, "/v1/messages", map[string]string{"authorization": "Bearer whatever"}, reqBody)

	if replayed.Code != http.StatusOK {
		t.Fatalf("replay: status = %d (body %s)\nlogs: %s", replayed.Code, replayed.Body, logs)
	}
	// byte-level equivalence between the live stream and its replay.
	if replayed.Body.String() != live.Body.String() {
		t.Errorf("replayed stream differs from the recorded one:\n--- live ---\n%s\n--- replay ---\n%s",
			live.Body.String(), replayed.Body.String())
	}
	if ct := replayed.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream — a client picks its parser from this", ct)
	}
	if n := rep.Snapshot().Replayed; n != 1 {
		t.Errorf("replayed = %d, want 1", n)
	}
	if n := rep.Snapshot().Upstream; n != 0 {
		t.Errorf("upstream calls = %d, want 0", n)
	}
}

// again, at event granularity: the client must see the same events in
// the same order, not one concatenated blob. A client that assembles deltas
// incrementally behaves differently against a single write, which is the whole
// reason the format stores a stream rather than a body.
func TestReplayPreservesEventFraming(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "framing")
	const reqBody = `{"model":"claude-sonnet-5","stream":true}`

	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseResponse)
	})
	post(t, rec, "/v1/messages", nil, reqBody)

	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {})
	fw := &framingWriter{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	rep.ServeHTTP(fw, r)

	if len(fw.writes) != 7 {
		t.Errorf("replay made %d writes, want one per SSE event (7): %q", len(fw.writes), fw.writes)
	}
	for i, wr := range fw.writes {
		if !strings.HasSuffix(wr, "\n\n") {
			t.Errorf("write %d does not end on an event boundary: %q", i, wr)
		}
	}
	if fw.flushes < len(fw.writes) {
		t.Errorf("flushed %d times for %d events — an unflushed event is one the client has not seen",
			fw.flushes, len(fw.writes))
	}
}

// framingWriter records each Write separately, which is the only way to tell a
// stream from a blob that happens to have the same bytes.
type framingWriter struct {
	*httptest.ResponseRecorder
	writes  []string
	flushes int
}

func (f *framingWriter) Write(p []byte) (int, error) {
	f.writes = append(f.writes, string(p))
	return f.ResponseRecorder.Write(p)
}
func (f *framingWriter) Flush() { f.flushes++; f.ResponseRecorder.Flush() }

// Non-streaming round trip, and the usage accounting that the shutdown summary needs to
// report what a replayed session would have cost.
func TestRecordThenReplayNonStreaming(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plain")
	const reqBody = `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
	const respBody = `{"id":"msg_01","content":[{"type":"text","text":"hello"}],"usage":{"input_tokens":12,"output_tokens":3}}`

	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	})
	post(t, rec, "/v1/messages", nil, reqBody)

	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	got := post(t, rep, "/v1/messages", nil, reqBody)
	if got.Body.String() != respBody {
		t.Errorf("replayed body = %s, want %s", got.Body, respBody)
	}

}

// A prompt that changed by one line is the everyday miss, and it must be
// legible. "Cassette miss" with nothing else is the single most likely cause of
// this tool being abandoned.
//
// The report names the step the session was at, what was recorded there, and
// the PATH that disagreed — not a line diff of two 60 KB bodies, which is what
// a nearest-match report degenerates into once a prompt is mostly shared
// boilerplate.
func TestMissNamesTheStepAndThePathThatDisagreed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "diffs")
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	post(t, rec, "/v1/messages", nil,
		`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"Refactor the auth module"}]}`)

	rep, logs := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	got := post(t, rep, "/v1/messages", nil,
		`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"Refactor the token store"}]}`)

	// 400, deliberately: a 5xx would be retried by the client, and a 404 on
	// this path is how the API says "no such model" — which is what Claude Code
	// then told its operator when a cassette missed.
	if got.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", got.Code)
	}
	body := got.Body.String()
	for _, want := range []string{"step 1", "/v1/messages", "auth modul", "token stor",
		"messages[0].content", "normalize.volatile"} {
		if !strings.Contains(body, want) {
			t.Errorf("miss message does not contain %q:\n%s", want, body)
		}
	}
	if !strings.Contains(logs.String(), "cassette miss") {
		t.Errorf("miss not logged: %s", logs)
	}
	if rep.Snapshot().Misses != 1 {
		t.Errorf("misses = %d, want 1", rep.Snapshot().Misses)
	}
}

// two requests the model would answer identically must match, even
// though the bytes differ. cache_control markers move with a client release and
// would otherwise invalidate every recording on upgrade.
func TestNormalizationIgnoresVolatileFields(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "norm")
	withCache := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`
	withoutCache := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`

	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	post(t, rec, "/v1/messages", nil, withCache)

	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a request differing only in cache_control was treated as new")
	})
	if got := post(t, rep, "/v1/messages", nil, withoutCache); got.Code != http.StatusOK {
		t.Fatalf("status = %d, want the cache_control difference to be normalized away (body %s)", got.Code, got.Body)
	}
}

// A transient failure is a step of the session like any other, and is flagged
// in the log because a cassette carrying a rate limit replays that rate limit —
// faithful, and also a sign the recording was made against a provider having a
// bad moment. See TestRetryAfterATransientFailureReachesTheProvider for what
// the client does with it on replay.
func TestTransientFailuresAreRecordedAndFlagged(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "errors")
	rec, logs := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	})
	got := post(t, rec, "/v1/messages", nil, `{"model":"claude-sonnet-5"}`)

	if got.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want upstream's own 429 passed back", got.Code)
	}
	if n := rec.Snapshot().Recorded; n != 1 {
		t.Errorf("recorded = %d, want 1 — replay has to have something to serve", n)
	}
	if !strings.Contains(logs.String(), "transient failure") {
		t.Errorf("the transient failure was recorded without a word: %s", logs)
	}
}

// A DETERMINISTIC failure is recorded, and replays. Claude Code probes an
// endpoint that 404s at startup; refusing to record that answer meant replay
// could never serve it, and the build failed on a request whose real answer was
// always going to be a 404.
func TestDeterministicFailuresAreRecordedAndReplayed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "notfound")
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not found"}`)
	})
	if got := post(t, rec, "/api/hello", nil, `{}`); got.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want upstream's 404 passed back", got.Code)
	}
	if n := rec.Snapshot().Recorded; n != 1 {
		t.Fatalf("recorded = %d, want the 404 recorded — it is deterministic", n)
	}

	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	got := post(t, rep, "/api/hello", nil, `{}`)
	if got.Code != http.StatusNotFound || got.Body.String() != `{"error":"not found"}` {
		t.Fatalf("status = %d body = %s, want the recorded 404 replayed", got.Code, got.Body)
	}
	if rep.Snapshot().Misses != 0 {
		t.Errorf("misses = %d, want the recorded 404 to be a hit", rep.Snapshot().Misses)
	}
}

// A recording session makes every call the session makes, including the ones
// it makes twice. Serving one from the cassette would leave a script with a
// turn missing from the middle of it, and a script with a hole in it replays
// as a divergence at the hole.
//
// This replaces "record fills gaps", which cannot survive an ordered script:
// there is no gap to fill in a sequence, only a different sequence.
func TestRecordingMakesEveryCallTheSessionMakes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "script")
	const same = `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"one"}]}`

	calls := 0
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprintf(w, `{"n":%d}`, calls)
	})
	post(t, rec, "/v1/messages", nil, same)
	post(t, rec, "/v1/messages", nil, same)

	if calls != 2 {
		t.Errorf("provider called %d times, want 2 — the second turn was served from the cassette", calls)
	}
	if n := rec.Snapshot().Recorded; n != 2 {
		t.Fatalf("recorded = %d, want both turns in the script", n)
	}

	// Two identical requests, two different answers, and replay has to give
	// them back in that order. Content addressing gave them one file and lost
	// the first answer; a script keeps both.
	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	if got := post(t, rep, "/v1/messages", nil, same); got.Body.String() != `{"n":1}` {
		t.Errorf("step 1 replayed as %s, want the first answer", got.Body)
	}
	if got := post(t, rep, "/v1/messages", nil, same); got.Body.String() != `{"n":2}` {
		t.Errorf("step 2 replayed as %s, want the second answer", got.Body)
	}
}

// Two bodiless requests to different paths are different interactions. Keying
// on the body alone made both the SHA-256 of the empty string, so a probe to
// /api/hello and any other bodiless request were one entry — found by running a
// real Claude Code build, whose startup probe collided this way.
func TestBodilessRequestsToDifferentPathsAreDifferentEntries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "probes")
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"path":"`+r.URL.Path+`"}`)
	})
	for _, p := range []string{"/api/hello", "/api/other"} {
		r := httptest.NewRequest(http.MethodGet, p, http.NoBody)
		rec.ServeHTTP(httptest.NewRecorder(), r)
	}
	if n := rec.Snapshot().Recorded; n != 2 {
		t.Fatalf("recorded = %d, want 2 — two paths must not share one entry", n)
	}

	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	for _, p := range []string{"/api/hello", "/api/other"} {
		w := httptest.NewRecorder()
		rep.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, http.NoBody))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", p, w.Code)
		}
		if !strings.Contains(w.Body.String(), p) {
			t.Errorf("%s replayed the wrong entry: %s", p, w.Body)
		}
	}
}

// A path cs-vcr does not model is still recorded and still replayed. It has to
// be: a request that is proxied but not recorded is one replay can never serve,
// so Claude Code's /api/hello probe would succeed while recording and error
// while replaying, and the run would diverge there.
func TestUnrecognizedSurfacesAreRecordedAndReplayed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "unknown-surface")
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"hello":true}`)
	})
	post(t, rec, "/api/hello", nil, `{}`)
	if n := rec.Snapshot().Recorded; n != 1 {
		t.Fatalf("recorded = %d, want the unrecognized surface recorded too", n)
	}

	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider for an unrecognized surface")
	})
	got := post(t, rep, "/api/hello", nil, `{}`)
	if got.Code != http.StatusOK || got.Body.String() != `{"hello":true}` {
		t.Fatalf("status = %d body = %s, want the recorded reply", got.Code, got.Body)
	}
}

// A miss must not be retryable. Stainless-generated SDKs — which is what Claude
// Code uses — retry 408, 409, 429 and every 5xx, which turns two misses into
// sixteen requests and hangs the run until its timeout.
func TestMissStatusIsNotRetryable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "retry")
	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	code := post(t, rep, "/v1/messages", nil, `{"model":"claude-sonnet-5"}`).Code
	for _, retryable := range []int{408, 409, 429, 500, 501, 502, 503, 504} {
		if code == retryable {
			t.Fatalf("miss returned %d, which clients retry — one miss becomes a storm", code)
		}
	}
	if code < 400 || code >= 500 {
		t.Errorf("miss returned %d, want a 4xx: it is the request that cannot be served, not the server failing", code)
	}
}

// A cassette must be text. Claude Code sends `Accept-Encoding: gzip`, and
// forwarding that verbatim made the recorder capture `1f 8b …` — unreadable in
// a PR diff, which is the entire reason for the format, and unreplayable,
// because the client fell back to non-streaming when the bytes would not parse.
func TestRecordedBodiesAreNotCompressed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "encoding")
	// An upstream that really compresses, so the test proves the outcome rather
	// than the mechanism: Go's transport adds its own Accept-Encoding and
	// decompresses transparently, which is what makes this work.
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			_, _ = io.WriteString(w, sseResponse)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = io.WriteString(gz, sseResponse)
		gz.Close()
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true}`))
	r.Header.Set("Accept-Encoding", "gzip")
	rec.ServeHTTP(httptest.NewRecorder(), r)

	entries, err := rec.cassette.Cassette().Entries()
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v (%v)", entries, err)
	}
	body, err := rec.cassette.Response(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		t.Fatal("the recorded body is gzip: a cassette that cannot be read in a diff is not a cassette")
	}
	if !strings.HasPrefix(string(body), "event: ") {
		t.Errorf("recorded body does not start with an SSE event: %.40q", body)
	}
}

// The recorded Content-Type is replayed verbatim. Guessing it is how a replayed
// stream stops looking like a stream to the client that has to parse it.
func TestReplayUsesTheRecordedContentType(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ctype")
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, sseResponse)
	})
	post(t, rec, "/v1/messages", nil, `{"stream":true}`)

	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	got := post(t, rep, "/v1/messages", nil, `{"stream":true}`)
	if ct := got.Header().Get("Content-Type"); ct != "text/event-stream; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the recorded one verbatim", ct)
	}
}

// A replayed response carries the recorded content type and nothing else.
//
// The rest of what a provider sends belongs to the recording run — a Set-Cookie
// holding its session, a Date, a Cloudflare request id — so putting them back
// hands the replaying client a stale timestamp and somebody else's cookie.
// Keeping them out of the cassette is also what makes "cs-vcr redacts nothing"
// safe rather than reckless: a session cookie committed to a repository is not
// something a later decision can take back.
func TestNeitherTheCassetteNorTheReplayCarriesTheRecordingRunsHeaders(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "headers")
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=RECORDING-RUN-SECRET; Path=/")
		w.Header().Set("Cf-Ray", "8e2f00000000-SEA")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	// The live client still gets them: cs-vcr is a recorder, and what upstream
	// sends is upstream's business on the way through.
	live := post(t, rec, "/v1/messages", nil, `{"model":"claude-sonnet-5"}`)
	if live.Header().Get("Set-Cookie") == "" {
		t.Error("a recording session withheld a header from the live client")
	}

	// It is what lands on disk that must not carry it.
	index, err := os.ReadFile(filepath.Join(dir, "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(index), "RECORDING-RUN-SECRET") {
		t.Errorf("the cassette holds the recording run's session cookie:\n%s", index)
	}

	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	got := post(t, rep, "/v1/messages", nil, `{"model":"claude-sonnet-5"}`)
	for _, h := range []string{"Set-Cookie", "Cf-Ray"} {
		if v := got.Header().Get(h); v != "" {
			t.Errorf("replay reproduced %s = %q", h, v)
		}
	}
	if ct := got.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want the one header a client does pick its parser from", ct)
	}
}

// A stream is a stream whether or not the upstream labels it. The Codex backend
// at chatgpt.com sends event frames under no Content-Type at all: believing the
// missing header stored the session as a JSON blob with no framing, and replayed
// it in one write to a client parsing for events.
func TestAnUnlabelledStreamIsStillRecordedAsAStream(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "unlabelled")
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		// nil rather than unset: Go would otherwise sniff a content type onto
		// the way out, and the upstream being modelled sends none.
		w.Header()["Content-Type"] = nil
		_, _ = io.WriteString(w, sseResponse)
	})
	post(t, rec, "/responses", nil, `{"stream":true}`)

	entries, err := rec.cassette.Cassette().Entries()
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v (%v)", entries, err)
	}
	if !entries[0].Streaming {
		t.Fatalf("entry recorded as non-streaming: %+v", entries[0])
	}
	if _, err := os.Stat(rec.cassette.Cassette().ResponsePath(entries[0].Seq, true)); err != nil {
		t.Errorf("stream was not stored as .sse: %v", err)
	}

	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	got := post(t, rep, "/responses", nil, `{"stream":true}`)
	if ct := got.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want a stream the client will parse as one", ct)
	}
	if got.Body.String() != sseResponse {
		t.Errorf("replayed body lost its framing:\n%q", got.Body)
	}
}

// The other half: an unlabelled body that is not a stream must not become one.
// A cassette entry claiming to stream is read from the wrong file and replayed
// event by event, so guessing wrong here breaks a response that worked.
func TestAnUnlabelledBodyIsNotMistakenForAStream(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "unlabelled-json")
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = nil
		_, _ = io.WriteString(w, `{"data":"not a stream","event":"neither"}`)
	})
	post(t, rec, "/responses", nil, `{"stream":false}`)

	entries, err := rec.cassette.Cassette().Entries()
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v (%v)", entries, err)
	}
	if entries[0].Streaming {
		t.Fatalf("a JSON body was recorded as a stream: %+v", entries[0])
	}
	if _, err := os.Stat(rec.cassette.Cassette().ResponsePath(entries[0].Seq, false)); err != nil {
		t.Errorf("body was not stored as .json: %v", err)
	}
}

// A client hangs up as soon as it has what it needs, and by then the response is
// already streaming: the ReverseProxy abandons the request the only way left to
// it, with panic(http.ErrAbortHandler), and net/http recovers that one silently.
// Everything after ServeHTTP was skipped, so the interaction was paid for
// upstream and left no entry, no log line and no error — a recording session
// whose summary read `upstream calls 2 / recorded 1` and said nothing else.
//
// Driven over a real socket, because a hang-up is the one thing an in-process
// ResponseWriter cannot model.
func TestAHangUpMidStreamStillRecordsWhatTheClientReceived(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hangup")

	// An upstream that streams, then holds the connection open: the only thing
	// that can end this response is the client leaving.
	held := make(chan struct{})
	defer close(held)
	rec, logs := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: a\ndata: {\"i\":0}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-held:
		case <-r.Context().Done():
		}
	})
	front := httptest.NewServer(rec)
	defer front.Close()

	resp, err := http.Post(front.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	// Read the event, then hang up — what an agent does once it has read the
	// last one of a turn.
	if line, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil || line != "event: a\n" {
		t.Fatalf("client read %q (%v), want the first event", line, err)
	}
	resp.Body.Close()

	// The entry lands as the abandoned handler unwinds, which is after the
	// client has gone.
	deadline := time.Now().Add(10 * time.Second)
	for rec.Snapshot().Recorded == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := rec.Snapshot().Recorded; n != 1 {
		t.Fatalf("recorded = %d, want the interaction the client hung up on\nlogs: %s", n, logs)
	}
	entries, err := rec.cassette.Cassette().Entries()
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v (%v)", entries, err)
	}
	if !entries[0].Streaming {
		t.Errorf("recorded as non-streaming: %+v", entries[0])
	}
	body, err := rec.cassette.Response(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "event: a") {
		t.Errorf("the recording is missing what the client received:\n%q", body)
	}
	// And it must not pass for a complete answer in the log, because from here
	// an answer cut off halfway looks exactly the same.
	if !strings.Contains(logs.String(), "client closed the connection") {
		t.Errorf("the interruption was not reported:\n%s", logs)
	}
}

// The other half, so the case above cannot be satisfied by warning on every
// recording: a response that ends by itself is complete, and saying otherwise
// on each of them would make the warning worth ignoring.
func TestACompletedResponseIsNotReportedAsInterrupted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "complete")
	rec, logs := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseResponse)
	})
	post(t, rec, "/v1/messages", nil, `{"model":"claude-sonnet-5","stream":true}`)

	if n := rec.Snapshot().Recorded; n != 1 {
		t.Fatalf("recorded = %d, want 1", n)
	}
	if strings.Contains(logs.String(), "interrupted response") {
		t.Errorf("a complete response was reported as interrupted:\n%s", logs)
	}
}

// Codex mints a session, thread, turn and window on every run and sends them in
// a telemetry block in the body, with the session id repeated as the prompt
// cache key and the day in its environment context. Left in the key, every turn
// of a recorded session missed on the first rerun — the whole cassette, always.
func TestCodexPerRunIdentifiersAreNotPartOfTheKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "codex")

	// The shape Codex sends, trimmed to what varies between two runs of it.
	body := func(session, turn, date, prompt string) string {
		return `{"model":"gpt-5.6-sol","client_metadata":{"session_id":"` + session +
			`","thread_id":"` + session + `","turn_id":"` + turn +
			`","x-codex-turn-metadata":"{\"turn_started_at_unix_ms\":1786674606129}"},` +
			`"prompt_cache_key":"` + session + `","input":[` +
			`{"role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <current_date>` +
			date + `</current_date>\n</environment_context>"}]},` +
			`{"role":"user","content":[{"type":"input_text","text":"` + prompt + `"}]}]}`
	}

	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	post(t, rec, "/responses", nil, body("019ffe21-4dee", "019ffe21-4e86", "2026-08-13", "add a /version endpoint"))
	if n := rec.Snapshot().Recorded; n != 1 {
		t.Fatalf("recorded = %d", n)
	}

	// A new session, on a new day: the same interaction.
	rep, logs := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider — normalization did not hold")
	})
	got := post(t, rep, "/responses", nil, body("019ffe22-14c9", "019ffe22-15b9", "2026-08-14", "add a /version endpoint"))
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want a hit across a new session and a new day\nlogs: %s", got.Code, logs)
	}

	// And the half that matters more: the prompt is still the key. Dropping the
	// block must not collapse two different asks into one interaction.
	miss := post(t, rep, "/responses", nil, body("019ffe22-14c9", "019ffe22-15b9", "2026-08-14", "delete the auth module"))
	if miss.Code == http.StatusOK {
		t.Errorf("a different prompt replayed the recorded answer: %s", miss.Body)
	}
}

// A multi-turn session breaks on the second turn, not the first, and this is
// why: Codex times its own tool calls and feeds the result back into the next
// request. The number cannot be the same twice, so turn one replays perfectly
// and turn two misses — which reads like the model having changed its mind.
//
// Under sequenced replay nothing rewrites those numbers. The tool RESULT is
// declared volatile, so a timing inside it is the world answering differently
// and the turn aligns with the timing left exactly as it arrived. Two rules
// that used to blank it were removed once this test passed without them.
func TestATurnIsNotKeyedOnHowLongTheLastToolCallTook(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "walltime")
	turn2 := func(chunk, jsonSecs, textSecs string) string {
		return `{"model":"gpt-5.6-sol","input":[` +
			`{"role":"user","content":[{"type":"input_text","text":"list the files"}]},` +
			`{"call_id":"call_01","type":"custom_tool_call","input":"ls internal"},` +
			`{"call_id":"call_01","output":[` +
			`{"type":"input_text","text":"{\"chunk_id\":\"` + chunk + `\",\"wall_time_seconds\":` + jsonSecs + `,\"exit_code\":0}"},` +
			`{"type":"input_text","text":"Script completed\nWall time ` + textSecs + ` seconds\nOutput:\n"},` +
			`{"type":"input_text","text":"cassette\ncli\nproxy\n"}]}]}`
	}

	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		// The model asking for the rest of that output, by the id it was given.
		_, _ = io.WriteString(w, `{"output":[{"type":"custom_tool_call","input":"read chunk 3f9a6b4670"}]}`)
	})
	post(t, rec, "/responses", nil, turn2("3f9a6b4670", "0.058013986", "0.2"))
	if n := rec.Snapshot().Recorded; n != 1 {
		t.Fatalf("recorded = %d", n)
	}

	// A rerun: the same command, timed differently, chunked under a new id.
	rep, logs := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider — the tool-call timing was left in the key")
	})
	got := post(t, rep, "/responses", nil, turn2("c1d2b11931", "0.052383862", "0.6"))
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want a hit across two timings of one command\nlogs: %s", got.Code, logs)
	}
	// And the chunk id has to come back as THIS run's, or the model is told to
	// read a chunk that only existed on the recording machine.
	if !strings.Contains(got.Body.String(), "read chunk c1d2b11931") {
		t.Errorf("the model was handed the recording run's chunk id:\n%s", got.Body)
	}

	// The half that matters more: what the agent DECIDED is exact. A tool call
	// that asked for something else is a different session, and no ruleset may
	// excuse it.
	other := strings.Replace(turn2("c1d2b11931", "0.052383862", "0.6"), `"ls internal"`, `"rm -rf /"`, 1)
	if miss := post(t, rep, "/responses", nil, other); miss.Code == http.StatusOK {
		t.Errorf("a different tool call replayed the recorded answer: %s", miss.Body)
	}
}

// A command that really did return something else is the WORLD having moved,
// not the session diverging: cs-vcr replays the model, and never claimed to
// reproduce what a shell prints. It is served, and reported, so an environment
// that drifted under a cassette is visible rather than silently absorbed.
func TestAChangedToolOutputIsServedAndReported(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "drift")
	turn := func(out string) string {
		return `{"model":"gpt-5.6-sol","input":[` +
			`{"role":"user","content":[{"type":"input_text","text":"list the files"}]},` +
			`{"call_id":"c1","type":"custom_tool_call","name":"exec","input":"ls internal"},` +
			`{"call_id":"c1","type":"custom_tool_call_output","output":[{"type":"input_text","text":"` + out + `"}]}]}`
	}

	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	post(t, rec, "/responses", nil, turn(`cassette\ncli\n`))

	rep, logs := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	got := post(t, rep, "/responses", nil, turn(`pyenv: cannot rehash\ncassette\ncli\n`))
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want the turn served despite the shell printing more\nlogs: %s", got.Code, logs)
	}
	if n := rep.Snapshot().Drifted; n != 1 {
		t.Errorf("drifted = %d, want the changed observation counted", n)
	}
	if !strings.Contains(logs.String(), "tolerated a changed observation") {
		t.Errorf("the drift was absorbed silently:\n%s", logs)
	}
}

// The other end of the same rule: the tool SCHEMA names these fields in its
// TypeScript declarations, and that text is stable prompt content the model
// reads. A pattern loose enough to rewrite it there would change a request that
// was never volatile.
func TestTheToolSchemaIsNotRewrittenByTheTimingRules(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "schema")
	const schema = `{"input":[{"role":"user","content":[{"type":"input_text",` +
		`"text":"chunk_id?: string;\n  exit_code?: number;\n  wall_time_seconds: number;\n"}]}]}`

	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	post(t, rec, "/responses", nil, schema)

	entries, err := rec.cassette.Cassette().Entries()
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v (%v)", entries, err)
	}
	b, err := os.ReadFile(rec.cassette.Cassette().RequestPath(entries[0].Seq))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"chunk_id?: string;", "wall_time_seconds: number;"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the tool schema was rewritten as if it were a value: %q missing from\n%s", want, b)
		}
	}
}

// The two things that actually break a real cassette, both measured against a
// real Claude Code run: the date and the working directory live in the PROMPT
// TEXT, where no amount of field stripping reaches them.
//
// Without normalizing them, a cassette recorded today misses every request
// tomorrow, and one recorded on a laptop misses every request in CI's checkout.
func TestPromptVolatilityIsNormalizedAway(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "volatile")

	// Exactly the shape Claude Code sends.
	body := func(date, cwd string) string {
		return `{"model":"claude-sonnet-5","messages":[{"role":"user","content":` +
			`"You are in the following environment: \n - Primary working directory: ` + cwd +
			`\n# currentDate\nToday's date is ` + date + `.\n"}]}`
	}

	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	post(t, rec, "/v1/messages", nil, body("2026-08-12", "/home/ada/scratch/project"))
	if n := rec.Snapshot().Recorded; n != 1 {
		t.Fatalf("recorded = %d", n)
	}

	// A different day, in a different checkout: the same interaction.
	rep, logs := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider — normalization did not hold")
	})
	got := post(t, rep, "/v1/messages", nil, body("2027-01-30", "/home/runner/work/repo/repo"))
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want a hit across a date and checkout change\nlogs: %s", got.Code, logs)
	}
	if rep.Snapshot().Replayed != 1 {
		t.Errorf("replayed = %d, want 1", rep.Snapshot().Replayed)
	}
}

// The normalized form is what lands on disk, so a reviewer sees the placeholder
// and knows the entry does not depend on the day it was recorded.
func TestNormalizedPlaceholdersAreVisibleInTheRecording(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "placeholders")
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	post(t, rec, "/v1/messages", nil,
		`{"messages":[{"role":"user","content":"Today's date is 2026-08-12."}]}`)

	entries, err := rec.cassette.Cassette().Entries()
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v (%v)", entries, err)
	}
	b, err := os.ReadFile(rec.cassette.Cassette().RequestPath(entries[0].Seq))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "2026-08-12") {
		t.Errorf("the recorded request still pins the date:\n%s", b)
	}
	if !strings.Contains(string(b), "<DATE>") {
		t.Errorf("the recorded request does not show the placeholder:\n%s", b)
	}
}

// A bad pattern is a startup error, not a per-request surprise.
func TestBadReplacementPatternFailsAtStartup(t *testing.T) {
	cfg := config.Default()
	cfg.Normalize.Replace = append(cfg.Normalize.Replace, config.Replacement{Pattern: "([unclosed"})
	if err := cfg.Resolve(); err == nil {
		t.Fatal("an invalid regex was accepted")
	}
}

// The checkout path is threaded through an agent's tool calls as absolute file
// paths, so normalizing the one line that names the working directory is not
// enough: a cassette recorded in one checkout still misses in another, on the
// tool calls.
func TestCheckoutRootIsNormalizedEverywhereItAppears(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "roots")
	body := func(root string) string {
		return `{"model":"claude-sonnet-5","messages":[` +
			`{"role":"assistant","content":[{"type":"tool_use","name":"Read",` +
			`"input":{"file_path":"` + root + `/project/README.md"}}]},` +
			`{"role":"user","content":[{"type":"tool_result","content":"read ` + root + `/project/README.md"}]}]}`
	}

	laptop, ci := "/home/ada/scratch/laptop", "/home/runner/work/repo/repo"
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	rec.cfg.Normalize.Root = laptop
	post(t, rec, "/v1/messages", nil, body(laptop))

	rep, logs := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider — the checkout root was not normalized")
	})
	rep.cfg.Normalize.Root = ci
	if got := post(t, rep, "/v1/messages", nil, body(ci)); got.Code != http.StatusOK {
		t.Fatalf("status = %d, want a hit across checkouts\nlogs: %s", got.Code, logs)
	}
}

// Tools derive per-directory state paths by turning the working directory's
// separators into dashes, so the checkout leaks back in a shape a literal path
// replacement cannot see. Claude Code's memory directory does exactly this, and
// it was the last thing keeping a real cassette from replaying in another
// checkout.
func TestSlugifiedCheckoutPathIsNormalized(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "slug")
	body := func(root string) string {
		slug := strings.ReplaceAll(root, "/", "-")
		return `{"messages":[{"role":"user","content":"memory at ~/.cs-claude/projects/` +
			slug + `/memory/ and files at ` + root + `/README.md"}]}`
	}
	laptop, ci := "/home/ada/scratch/lap", "/home/runner/work/repo/repo"

	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	rec.cfg.Normalize.Root = laptop
	post(t, rec, "/v1/messages", nil, body(laptop))

	rep, logs := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider — the slugified checkout was not normalized")
	})
	rep.cfg.Normalize.Root = ci
	if got := post(t, rep, "/v1/messages", nil, body(ci)); got.Code != http.StatusOK {
		t.Fatalf("status = %d, want a hit across checkouts\nlogs: %s", got.Code, logs)
	}
}

// A recorded RESPONSE carries paths too, and the client acts on them: a tool
// call names an absolute file path, the client echoes it back in its next
// request, and a response holding the recording machine's paths makes every
// follow-up unmatchable. This was the last thing keeping a real agent loop from
// replaying in a different checkout.
func TestResponsePathsRoundTripThroughTheCassette(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "roundtrip")
	laptop, ci := "/home/ada/scratch/lap", "/home/runner/work/repo/repo"

	// Record: the model answers with a tool call naming an absolute path.
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"content":[{"type":"tool_use","input":{"file_path":"`+laptop+`/project/README.md"}}]}`)
	})
	rec.cfg.Normalize.Root = laptop
	post(t, rec, "/v1/messages", nil, `{"messages":[{"role":"user","content":"read it"}]}`)

	// The cassette must hold the placeholder, or it is not portable.
	entries, err := rec.cassette.Cassette().Entries()
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v (%v)", entries, err)
	}
	stored, err := os.ReadFile(rec.cassette.Cassette().ResponsePath(entries[0].Seq, false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), laptop) {
		t.Errorf("the recorded response pins the recording machine's path:\n%s", stored)
	}

	// Replay elsewhere: the client must receive paths that exist HERE.
	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	rep.cfg.Normalize.Root = ci
	got := post(t, rep, "/v1/messages", nil, `{"messages":[{"role":"user","content":"read it"}]}`)
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d", got.Code)
	}
	if !strings.Contains(got.Body.String(), ci+"/project/README.md") {
		t.Errorf("replayed response does not carry this checkout's path: %s", got.Body)
	}
	if strings.Contains(got.Body.String(), "<ROOT>") {
		t.Errorf("the placeholder leaked to the client: %s", got.Body)
	}
}

// The case that blocked cross-machine replay twice: a model's tool call names
// a path, the fragments split it mid-string, and the client acts on whatever it
// receives. Recorded on one checkout, replayed on another, the client must be
// handed ITS OWN path — not the recording machine's, which does not exist here.
func TestToolCallPathIsRewrittenForTheReplayingMachine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "toolpath")
	laptop, ci := "/home/ada/scratch/lap", "/home/runner/work/repo/repo"

	// A tool call streamed the way a model really sends it: split mid-path.
	stream := func(root string) string {
		p := root + "/project/README.md"
		return "event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"Read","input":{}}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\": \"` + p[:12] + `"}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"` + p[12:] + `\"}"}}` + "\n\n" +
			"event: content_block_stop\n" +
			`data: {"type":"content_block_stop","index":0}` + "\n\n"
	}

	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream(laptop))
	})
	rec.cfg.Normalize.Root = laptop
	post(t, rec, "/v1/messages", nil, `{"messages":[{"role":"user","content":"read it"}]}`)

	// The cassette must hold neither the laptop's path nor a split one.
	entries, err := rec.cassette.Cassette().Entries()
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v (%v)", entries, err)
	}
	stored, err := os.ReadFile(rec.cassette.Cassette().ResponsePath(entries[0].Seq, true))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), laptop) {
		t.Fatalf("the recording still pins the recording machine's path:\n%s", stored)
	}
	if !strings.Contains(string(stored), "<ROOT>/project/README.md") {
		t.Fatalf("the path was not joined and normalized:\n%s", stored)
	}

	// Replayed elsewhere, the client gets a path that exists HERE.
	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	rep.cfg.Normalize.Root = ci
	got := post(t, rep, "/v1/messages", nil, `{"messages":[{"role":"user","content":"read it"}]}`)
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d", got.Code)
	}
	if !strings.Contains(got.Body.String(), ci+"/project/README.md") {
		t.Fatalf("the client was not handed its own path:\n%s", got.Body)
	}
	if strings.Contains(got.Body.String(), "<ROOT>") {
		t.Errorf("a placeholder leaked to the client:\n%s", got.Body)
	}
}

// The campaign case: a per-run identifier that appears in the prompt AND in a
// path the agent is told to open. Blanking it makes the request match; the
// value has to come BACK on the way out, or the agent opens the recording run's
// file, which does not exist.
func TestCapturedIdentifierRoundTripsIntoTheResponse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dispatch")
	req := func(id string) string {
		return `{"messages":[{"role":"user","content":"Read ~/input/` + id + `.md and follow it. Dispatch ID: ` + id + `"}]}`
	}
	resp := func(id string) string {
		return `{"content":[{"type":"tool_use","input":{"file_path":"/home/user/input/` + id + `.md"}}]}`
	}

	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, resp("dispatch-111111111111"))
	})
	rec.cfg.Normalize.Capture = []config.Capture{{Pattern: `dispatch-[0-9]{10,}`, As: "<DISPATCH>"}}
	if err := rec.cfg.Normalize.Compile(); err != nil {
		t.Fatal(err)
	}
	post(t, rec, "/v1/messages", nil, req("dispatch-111111111111"))
	if n := rec.Snapshot().Recorded; n != 1 {
		t.Fatalf("recorded = %d", n)
	}

	// A different run, a different id: it must still match, and the response
	// must name the NEW id.
	rep, logs := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	rep.cfg.Normalize.Capture = rec.cfg.Normalize.Capture
	if err := rep.cfg.Normalize.Compile(); err != nil {
		t.Fatal(err)
	}
	got := post(t, rep, "/v1/messages", nil, req("dispatch-999999999999"))
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want a hit across differing ids\nlogs: %s", got.Code, logs)
	}
	if !strings.Contains(got.Body.String(), "dispatch-999999999999") {
		t.Errorf("the response names the recording run's id, not this one:\n%s", got.Body)
	}
	if strings.Contains(got.Body.String(), "dispatch-111111111111") {
		t.Errorf("the recording run's id leaked to the client:\n%s", got.Body)
	}
}

// A capture pattern usually needs context to be safe: the run-specific part is
// a bare identifier, and matching it alone would collapse every unrelated one
// in the request into the same placeholder. With a group, only the group is
// blanked and the surrounding text stays.
func TestCaptureGroupScopesWhatIsBlanked(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scoped")
	body := func(session string) string {
		// Two uuids: one per-run, one that is part of the recorded content and
		// must NOT be touched.
		return `{"messages":[{"role":"user","content":"scratch at /tmp/agent/` + session +
			`/work and ticket 11111111-2222-3333-4444-555555555555"}]}`
	}
	capture := []config.Capture{{Pattern: `(?:/tmp/agent/)([0-9a-f-]{36})`, As: "<SESSION>"}}

	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	rec.cfg.Normalize.Capture = capture
	if err := rec.cfg.Normalize.Compile(); err != nil {
		t.Fatal(err)
	}
	post(t, rec, "/v1/messages", nil, body("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"))

	entries, _ := rec.cassette.Cassette().Entries()
	stored, err := os.ReadFile(rec.cassette.Cassette().RequestPath(entries[0].Seq))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored), "/tmp/agent/<SESSION:1>/work") {
		t.Errorf("the group was not blanked in place:\n%s", stored)
	}
	if !strings.Contains(string(stored), "11111111-2222-3333-4444-555555555555") {
		t.Errorf("an unrelated uuid was swallowed by the capture:\n%s", stored)
	}

	rep, logs := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	rep.cfg.Normalize.Capture = capture
	if err := rep.cfg.Normalize.Compile(); err != nil {
		t.Fatal(err)
	}
	if got := post(t, rep, "/v1/messages", nil, body("99999999-8888-7777-6666-555555555555")); got.Code != http.StatusOK {
		t.Fatalf("status = %d, want a hit across differing session ids\nlogs: %s", got.Code, logs)
	}
}

// One pattern, several distinct values, and they are not interchangeable.
//
// An orchestrator delegating to a member holds two dispatch ids: its own, and
// the one it minted for the member. Collapsing both into a single placeholder
// restored them to the same value, and the replayed orchestrator then polled a
// session it had never prompted — a stall, not a miss, so the cassette looked
// clean while the campaign hung.
func TestSeveralDistinctValuesOfOnePatternRoundTripSeparately(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "twoids")
	req := func(mine, theirs string) string {
		return `{"messages":[{"role":"user","content":"my dispatch is ` + mine +
			`; I sent worker ` + theirs + ` and I am polling ` + theirs + `"}]}`
	}
	resp := func(mine, theirs string) string {
		return `{"content":[{"type":"tool_use","input":{"cmd":"status ` + theirs +
			` && finish ` + mine + `"}}]}`
	}
	capture := []config.Capture{{Pattern: `dispatch-[0-9]{10,}`, As: "<DISPATCH>"}}

	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, resp("dispatch-1111111111", "dispatch-2222222222"))
	})
	rec.cfg.Normalize.Capture = capture
	if err := rec.cfg.Normalize.Compile(); err != nil {
		t.Fatal(err)
	}
	post(t, rec, "/v1/messages", nil, req("dispatch-1111111111", "dispatch-2222222222"))

	rep, logs := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	rep.cfg.Normalize.Capture = capture
	if err := rep.cfg.Normalize.Compile(); err != nil {
		t.Fatal(err)
	}
	got := post(t, rep, "/v1/messages", nil, req("dispatch-3333333333", "dispatch-4444444444"))
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want a hit\nlogs: %s", got.Code, logs)
	}
	body := got.Body.String()
	// Each id must come back as ITSELF: polling must name the worker's id and
	// finishing must name the orchestrator's.
	if !strings.Contains(body, "status dispatch-4444444444") {
		t.Errorf("the worker's id was not restored to itself:\n%s", body)
	}
	if !strings.Contains(body, "finish dispatch-3333333333") {
		t.Errorf("the orchestrator's id was not restored to itself:\n%s", body)
	}
	if strings.Contains(body, "dispatch-1111111111") || strings.Contains(body, "dispatch-2222222222") {
		t.Errorf("a recording run's id leaked to the client:\n%s", body)
	}
}

// A recording has to survive the agent updating itself. Codex opens every
// session with `GET /v1/models?client_version=<its own build>`, so without the
// query strip list the first request after an upgrade misses — and a miss on
// the model list fails the pipeline exactly like a miss on a prompt, while
// looking like a prompt that changed.
func TestReplaySurvivesAnAgentUpdate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "codex")
	const models = `{"data":[{"id":"gpt-5.6-sol"}]}`

	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, models)
	})
	if got := get(t, rec, "/v1/models?client_version=0.145.0"); got.Code != http.StatusOK {
		t.Fatalf("record: status = %d", got.Code)
	}

	// The upstream fails the test if it is dialled: replay must serve the
	// updated agent from the cassette, not fetch a fresh list for it.
	rep, logs := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	replayed := get(t, rep, "/v1/models?client_version=0.146.0")
	if replayed.Code != http.StatusOK {
		t.Fatalf("replay after an agent update: status = %d (body %s)\nlogs: %s", replayed.Code, replayed.Body, logs)
	}
	if replayed.Body.String() != models {
		t.Errorf("replayed body = %s, want the recorded list", replayed.Body)
	}
	if n := rep.Snapshot().Replayed; n != 1 {
		t.Errorf("replayed = %d, want 1", n)
	}
}

// The other half: only the named parameters are ignored. A query that selects
// what the provider does still separates two interactions, and replay says so
// rather than serving one for the other.
func TestReplayStillMissesOnAQueryThatMatters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "beta")
	const reqBody = `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`

	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	post(t, rec, "/v1/messages?beta=true", nil, reqBody)

	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	missed := post(t, rep, "/v1/messages", nil, reqBody)
	if missed.Code == http.StatusOK {
		t.Fatalf("a request without ?beta=true was served the beta recording: %s", missed.Body)
	}
	if n := rep.Snapshot().Misses; n != 1 {
		t.Errorf("misses = %d, want 1", n)
	}
}

// A miss offers the `volatile` remedy only where it is one.
//
// A value that differs may well be the world's answer, and saying so saves a
// reader the round trip. An item added or removed is the request being BUILT
// differently, and the path of the list it happened in is the prompt itself —
// advising anyone to declare that volatile would send them to blank the thing
// they are trying to match on.
func TestTheVolatileRemedyIsOnlyOfferedWhereItIsOne(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "advice")
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	post(t, rec, "/responses", nil, `{"input":[{"content":[{"text":"a"},{"text":"b"}]}],"seen":"x"}`)

	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	// A value differs: the remedy applies.
	valued := post(t, rep, "/responses", nil, `{"input":[{"content":[{"text":"a"},{"text":"b"}]}],"seen":"y"}`)
	if !strings.Contains(valued.Body.String(), "normalize.volatile") {
		t.Errorf("a changed value was reported without the remedy:\n%s", valued.Body)
	}

	// A block went missing: it does not.
	rep2, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	shaped := post(t, rep2, "/responses", nil, `{"input":[{"content":[{"text":"a"}]}],"seen":"x"}`)
	if strings.Contains(shaped.Body.String(), "normalize.volatile") {
		t.Errorf("a missing prompt block was answered with advice to blank the prompt:\n%s", shaped.Body)
	}
	if !strings.Contains(shaped.Body.String(), "2 items vs 1") {
		t.Errorf("the missing block was not named:\n%s", shaped.Body)
	}
}

// A miss on a request that carries a query says what actually differed.
//
// The entry records a TARGET — path and query — so a report that holds a bare
// path beside it declares every query parameter a changed endpoint. Claude Code
// asks for its beta surface with `?beta=true` on every request, so this turned
// every prompt difference in a real session into "this run asked for
// /v1/messages instead", and sent the reader hunting a routing fault that was
// not there.
func TestAMissOnAQueriedTargetNamesWhatDiffered(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "queried")
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	post(t, rec, "/v1/messages?beta=true", nil, `{"model":"claude-sonnet-5","system":"a"}`)

	// Same target, changed body: the report must name the field.
	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	body := post(t, rep, "/v1/messages?beta=true", nil, `{"model":"claude-sonnet-5","system":"b"}`).Body.String()
	if strings.Contains(body, "asked for") {
		t.Errorf("a changed body was reported as a changed target:\n%s", body)
	}
	if !strings.Contains(body, "system") {
		t.Errorf("the field that differed was not named:\n%s", body)
	}

	// A target that really did change still reports as one, with the query on
	// both sides so the two can be compared.
	rep2, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	moved := post(t, rep2, "/v1/messages", nil, `{"model":"claude-sonnet-5","system":"a"}`).Body.String()
	if !strings.Contains(moved, "asked for /v1/messages instead") {
		t.Errorf("a changed target was not reported as one:\n%s", moved)
	}
}

// A recorded rate limit replays with the wait it was recorded with.
//
// A transient failure is a step of the session, so the client retries it on
// replay exactly as it did on the recording. What decides how long it waits
// first is `Retry-After`. Drop it and the client falls back to its own default
// — usually a shorter one — so the replayed run has a different shape from the
// one on the tape, in the one place a cassette exists to reproduce.
func TestARecordedRateLimitReplaysWithItsRetryAfter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "throttled")
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error"}`)
	})
	post(t, rec, "/v1/messages", nil, `{"model":"claude-sonnet-5"}`)

	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	got := post(t, rep, "/v1/messages", nil, `{"model":"claude-sonnet-5"}`)
	if got.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the recorded 429", got.Code)
	}
	if v := got.Header().Get("Retry-After"); v != "3" {
		t.Errorf("Retry-After = %q, want the recorded 3", v)
	}
}

// The carve-out is exactly as wide as its reason, and no wider.
//
// A `Retry-After` on a status nothing retries is a note about a quota the
// caller is not blocked by, and an HTTP-date is a moment in the recording run:
// replayed, its deadline has always already passed, so a client computes a
// negative wait and retries at once — the opposite of the backoff recorded.
// Neither is kept, and neither leaves the recording run's clock in a cassette.
func TestOnlyARetryableStatusWithADelayKeepsItsRetryAfter(t *testing.T) {
	cases := []struct {
		name   string
		status int
		value  string
	}{
		{"success", http.StatusOK, "3"},
		{"client error", http.StatusNotFound, "3"},
		{"http-date on a rate limit", http.StatusTooManyRequests, "Wed, 12 Aug 2026 18:04:11 GMT"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "narrow")
			rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", c.value)
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, `{"ok":true}`)
			})
			post(t, rec, "/v1/messages", nil, `{"model":"claude-sonnet-5"}`)

			index, err := os.ReadFile(filepath.Join(dir, "index.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(index), "retry_after") {
				t.Errorf("the cassette kept a Retry-After it will not replay:\n%s", index)
			}

			rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
				t.Error("replay contacted the provider")
			})
			got := post(t, rep, "/v1/messages", nil, `{"model":"claude-sonnet-5"}`)
			if v := got.Header().Get("Retry-After"); v != "" {
				t.Errorf("replay reproduced Retry-After = %q", v)
			}
		})
	}
}
