package proxy

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codesweep-ai/vcr/internal/cassette"
	"github.com/codesweep-ai/vcr/internal/config"
)

// These cover what a real cs-campaign run puts through the proxy, taken from
// its recorded cassettes rather than from imagination:
//
//	HEAD /api/hello          unknown surface, no body, 404
//	POST /v1/messages?beta=true   streamed, 200, request bodies over 160 KB
//	two members at once      one proxy, concurrent sessions

// The query selects provider behaviour — Claude Code asks for beta features
// with `?beta=true` — so one body under two queries is two interactions. Keying
// on the path alone recorded them as one, and replay would then answer a beta
// request with a non-beta recording.
func TestQueryStringIsPartOfTheKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "query")
	var seen []string
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RequestURI())
		_, _ = io.WriteString(w, `{"q":"`+r.URL.RawQuery+`"}`)
	})
	for _, q := range []string{"?beta=true", ""} {
		r := httptest.NewRequest(http.MethodPost, onCassette("/v1/messages"+q), strings.NewReader(`{"m":1}`))
		rec.ServeHTTP(httptest.NewRecorder(), r)
	}
	if n := sessionStore(t, rec).Len(); n != 2 {
		t.Fatalf("distinct entries = %d, want 2 — the query is part of the interaction", n)
	}
	// And it must reach the provider, or the beta features were silently
	// dropped on the way through.
	if len(seen) != 2 || seen[0] != "/v1/messages?beta=true" {
		t.Fatalf("upstream saw %v, want the query forwarded", seen)
	}

	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	for _, tc := range []struct{ query, want string }{
		{"?beta=true", `{"q":"beta=true"}`},
		{"", `{"q":""}`},
	} {
		w := httptest.NewRecorder()
		rep.ServeHTTP(w, httptest.NewRequest(http.MethodPost, onCassette("/v1/messages"+tc.query), strings.NewReader(`{"m":1}`)))
		if w.Body.String() != tc.want {
			t.Errorf("query %q replayed %s, want %s", tc.query, w.Body, tc.want)
		}
	}
}

// The probe a real campaign sends at startup: HEAD, no request body, and a
// response with no body either. It has to record and replay like anything
// else — an unrecorded probe is a miss that fails the run.
func TestHeadRequestRoundTrips(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "head")
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	w := httptest.NewRecorder()
	rec.ServeHTTP(w, httptest.NewRequest(http.MethodHead, onCassette("/api/hello"), http.NoBody))
	if w.Code != http.StatusNotFound {
		t.Fatalf("record: status = %d", w.Code)
	}
	if n := rec.Snapshot().Recorded; n != 1 {
		t.Fatalf("recorded = %d, want the probe recorded", n)
	}

	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	w = httptest.NewRecorder()
	rep.ServeHTTP(w, httptest.NewRequest(http.MethodHead, onCassette("/api/hello"), http.NoBody))
	if w.Code != http.StatusNotFound {
		t.Errorf("replay: status = %d, want the recorded 404", w.Code)
	}
	if rep.Snapshot().Misses != 0 {
		t.Errorf("misses = %d, want the probe served from the cassette", rep.Snapshot().Misses)
	}
}

// A GET and a HEAD of one path are different interactions, and a campaign sends
// both shapes of bodiless request. Keying on the body alone made every bodiless
// request the same entry.
func TestMethodIsPartOfTheKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "method")
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"m":"`+r.Method+`"}`)
	})
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
		rec.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(m, onCassette("/api/hello"), http.NoBody))
	}
	if n := sessionStore(t, rec).Len(); n != 3 {
		t.Fatalf("distinct entries = %d, want one per method", n)
	}
}

// A real campaign request is over 160 KB of accumulated conversation. Nothing
// should truncate it — not the recorder, not the index reader, not the diff.
func TestLargeRequestBodyRoundTrips(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "large")
	big := `{"messages":[{"role":"user","content":"` + strings.Repeat("conversation history. ", 9000) + `"}]}`
	if len(big) < 160_000 {
		t.Fatalf("fixture is only %d bytes; the case being covered is bigger", len(big))
	}
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	post(t, rec, "/v1/messages", nil, big)

	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	if got := post(t, rep, "/v1/messages", nil, big); got.Code != http.StatusOK {
		t.Fatalf("status = %d, want a 160 KB body to round-trip", got.Code)
	}
}

// A campaign runs several members against one proxy at once. The cassette is
// written and read concurrently, and a torn index or a lost entry would show up
// only under load — which is exactly when nobody is watching.
func TestConcurrentMembersShareOneCassette(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "concurrent")
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	const members, turns = 4, 5
	var wg sync.WaitGroup
	for m := range members {
		wg.Go(func() {
			for i := range turns {
				body := fmt.Sprintf(`{"member":%d,"turn":%d}`, m, i)
				r := httptest.NewRequest(http.MethodPost, onCassette("/v1/messages"), strings.NewReader(body))
				rec.ServeHTTP(httptest.NewRecorder(), r)
			}
		})
	}
	wg.Wait()

	if n := sessionStore(t, rec).Len(); n != members*turns {
		t.Fatalf("distinct entries = %d, want %d — an entry was lost under concurrency", n, members*turns)
	}
	// The index must still parse: a torn line would make the whole cassette
	// unreadable on the next run.
	entries, err := sessionStore(t, rec).Cassette().Entries()
	if err != nil {
		t.Fatalf("index is unreadable after concurrent writes: %v", err)
	}
	if len(entries) != members*turns {
		t.Errorf("index holds %d entries, want %d", len(entries), members*turns)
	}

	// And every one of them replays — in the order the interleaving actually
	// produced, which is what a cassette is: the session as it happened, not
	// as any one member would retell it.
	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	for _, e := range entries {
		body, err := os.ReadFile(sessionStore(t, rep).Cassette().RequestPath(e.Seq))
		if err != nil {
			t.Fatal(err)
		}
		if got := post(t, rep, "/v1/messages", nil, string(body)); got.Code != http.StatusOK {
			t.Fatalf("step %d: status = %d", e.Seq, got.Code)
		}
	}
	if rep.Snapshot().Misses != 0 {
		t.Errorf("misses = %d after a concurrent recording", rep.Snapshot().Misses)
	}
}

// A model can call several tools in one turn, and the client returns their
// results in whatever order they finish. A real campaign missed on replay for
// exactly this: identical content, flipped order — a small file came back
// before a large one on one run and after it on the next.
//
// The order carries no information; tool_use_id binds each result to its call.
func TestParallelToolResultsMatchInAnyOrder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "parallel")
	// The same two results, assembled in the two orders a client might produce.
	body := func(first, second string) string {
		return `{"messages":[{"role":"user","content":[` +
			`{"type":"tool_result","tool_use_id":"toolu_A","content":"` + first + `"},` +
			`{"type":"tool_result","tool_use_id":"toolu_B","content":"` + second + `"}]}]}`
	}
	const a, b = "contents of stats.py", "File does not exist."

	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	post(t, rec, "/v1/messages", nil, body(a, b))

	rep, logs := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider — the result order was treated as meaning")
	})
	// Same results, other order: the ids still say which answers which.
	swapped := `{"messages":[{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"toolu_B","content":"` + b + `"},` +
		`{"type":"tool_result","tool_use_id":"toolu_A","content":"` + a + `"}]}]}`
	if got := post(t, rep, "/v1/messages", nil, swapped); got.Code != http.StatusOK {
		t.Fatalf("status = %d, want a hit regardless of completion order\nlogs: %s", got.Code, logs)
	}
}

// Blocks that are not tool_result must not move: for those, position is
// meaning, and a reordered conversation is a different conversation.
func TestOnlyToolResultsAreReordered(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "order")
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	post(t, rec, "/v1/messages", nil,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}]}`)

	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	got := post(t, rep, "/v1/messages", nil,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"second"},{"type":"text","text":"first"}]}]}`)
	if got.Code == http.StatusOK {
		t.Fatal("two differently ordered text blocks were treated as the same conversation")
	}
}

// Two campaign members, one proxy, and the same opening prompt.
//
// This is the shape that broke a whole recorded campaign. cs-campaign hands
// every member the same instruction — "read this dispatch file and follow it,
// Dispatch ID: <id>" — differing only in the id, which normalization blanks so
// that the pair replays at all. Both members' first requests then normalize to
// identical bytes.
//
// Sharing one cassette means sharing a key, and the damage lands while
// RECORDING: a hit is served from the cassette rather than fetched again, so
// the second member is handed the first member's response and never reaches the
// provider. One request is recorded where two were sent, and the cassette reads
// as complete until replay, where the members diverge on turn two.
//
// A cassette per member is what keeps them apart, and the prefix on each
// member's base URL is the whole of how that is arranged.
func TestTwoMembersWithIdenticalRequestsDoNotShareEntries(t *testing.T) {
	// Byte-identical, as they are after normalization blanks the dispatch id.
	const sameBody = `{"model":"claude-opus-5","messages":[{"role":"user","content":"read the dispatch file and follow it"}]}`

	root := t.TempDir()

	// --- Record: each member must reach the provider on its own account.
	var calls int
	rec := prefixCassetteServer(t, root, online, func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprintf(w, `{"reply":"response %d"}`, calls)
	})
	first := post(t, rec, "/c/orchestrator/v1/messages", nil, sameBody)
	second := post(t, rec, "/c/worker/v1/messages", nil, sameBody)

	if calls != 2 {
		t.Fatalf("provider called %d times for two members, want 2 — one was served the other's recording", calls)
	}
	if first.Body.String() == second.Body.String() {
		t.Fatalf("both members got %s — their traffic was recorded as one interaction", first.Body)
	}

	// --- Replay: each member gets back its own response, not the other's.
	rep := prefixCassetteServer(t, root, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	for _, c := range []struct{ prefix, want string }{
		{"/c/orchestrator", first.Body.String()},
		{"/c/worker", second.Body.String()},
	} {
		got := post(t, rep, c.prefix+"/v1/messages", nil, sameBody)
		if got.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (%s)", c.prefix, got.Code, got.Body)
		}
		if got.Body.String() != c.want {
			t.Errorf("%s replayed %s, want %s — the members' recordings crossed",
				c.prefix, got.Body, c.want)
		}
	}
}

// clientCassetteServer is cassetteServer with several clients, each with its own
// cassette under root.
// prefixCassetteServer builds a proxy that opens a cassette per name as the
// prefixes ask for it, which is what one cs-vcr serving several agents does.
func prefixCassetteServer(t *testing.T, root string, offline bool, upstream http.HandlerFunc) *Server {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)

	cfg := config.Default()
	cfg.Providers["anthropic"] = &config.Provider{BaseURL: up.URL}
	cfg.Providers["openai"] = &config.Provider{BaseURL: up.URL}
	if err := cfg.Resolve(); err != nil {
		t.Fatal(err)
	}
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), offline)
	s = s.WithOpener(func(name string) (*cassette.Store, error) {
		return cassette.OpenStore(filepath.Join(root, name), "test",
			cfg.Normalize.Version, func() int64 { return 0 })
	})
	s.now = func() time.Time { return time.Unix(0, 0).UTC() }
	return s
}

// A rate-limited probe must not become a permanent hole in the cassette.
//
// Claude Code opens a session with a one-token "quota" probe. When that probe
// is rate-limited during recording it is still something that happened, and
// refusing to record it meant replay missed it every time — and a miss is an
// error the client had never seen at that point in the session, so it diverged
// from there. It is recorded, and replay serves what the recording saw.
func TestTransientFailureIsRecordedAndReplayed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ratelimited")
	const probe = `{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`

	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate_limited"}`)
	})
	if got := post(t, rec, "/v1/messages", nil, probe); got.Code != http.StatusTooManyRequests {
		t.Fatalf("record: status = %d, want the provider's own 429", got.Code)
	}

	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	got := post(t, rep, "/v1/messages", nil, probe)
	if got.Code == http.StatusBadRequest {
		t.Fatalf("replay missed a request that was recorded: %s", got.Body)
	}
	if got.Code != http.StatusTooManyRequests {
		t.Errorf("replay served %d, want the recorded 429", got.Code)
	}
}

// The other half of recording it: while recording, a retry must still reach the
// provider. The retry is the same request and so the same key, so serving the
// cassette would hand the client back the very failure it was retrying — and
// the recording would freeze a rate limit into a cassette that CI then replays
// forever.
func TestRetryAfterATransientFailureReachesTheProvider(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "retried")
	const req = `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`

	var calls int
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"rate_limited"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	post(t, rec, "/v1/messages", nil, req)
	retry := post(t, rec, "/v1/messages", nil, req)

	if calls != 2 {
		t.Fatalf("provider called %d times, want 2 — the retry was served the failure it was retrying", calls)
	}
	if retry.Body.String() != `{"ok":true}` {
		t.Fatalf("retry got %s, want the successful response", retry.Body)
	}

	// Replay reproduces the session, which is both of those steps: the client
	// meets the same rate limit and retries it the same way. Collapsing them
	// into one entry and serving the success would replay a session that never
	// happened — and would hide, from a run that is supposed to be faithful,
	// that the recording was made against a provider that was rate-limiting.
	rep, _ := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	first := post(t, rep, "/v1/messages", nil, req)
	if first.Code != http.StatusTooManyRequests {
		t.Errorf("step 1 replayed as %d, want the 429 the session met", first.Code)
	}
	second := post(t, rep, "/v1/messages", nil, req)
	if second.Code != http.StatusOK || second.Body.String() != `{"ok":true}` {
		t.Errorf("step 2 replayed as %d %s, want the success the retry got", second.Code, second.Body)
	}
	if n := rep.Snapshot().Misses; n != 0 {
		t.Errorf("misses = %d, want the retry to be a step of its own", n)
	}
}

// A cassette that pins its provider settles every request on its prefix.
//
// The prefix is a base URL the client was configured with, and a client
// configures one base URL per provider, so this is the fact the deployment
// already knows. Asserted with the request that has nothing else to go on:
// Claude Code's startup probe carries the prefix but no identifying header, and
// inferring from the rest of it sent an Anthropic-only user to api.openai.com.
func TestAPinnedProviderDecidesEveryPathOnItsPrefix(t *testing.T) {
	var got []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(up.Close)

	cfg := config.Default()
	cfg.Providers["anthropic"] = &config.Provider{BaseURL: up.URL}
	// Reaching this one fails the test: nothing on the prefix may go here.
	cfg.Providers["openai"] = &config.Provider{BaseURL: "http://127.0.0.1:1"}
	cfg.CassetteProvider = map[string]string{"feature": "anthropic"}
	cfg.DefaultProvider = "openai" // even so, the pin wins
	if err := cfg.Resolve(); err != nil {
		t.Fatal(err)
	}
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), online)

	// The probe, exactly as captured: prefix present, nothing else to go on.
	for _, path := range []string{"/c/feature/api/hello", "/c/feature/v1/messages"} {
		r := httptest.NewRequest(http.MethodHead, onCassette(path), http.NoBody)
		r.Header.Set("user-agent", "Bun/1.4.0")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		got = append(got, path)
		if w.Code == http.StatusBadGateway {
			t.Errorf("%s reached the wrong provider: %d %s", path, w.Code, w.Body)
		}
	}
	if len(got) != 2 {
		t.Fatalf("sent %d requests", len(got))
	}
}

// A pinned provider that is not configured is a typo, and it is refused at
// startup rather than surfacing as a 502 on the first recorded request.
func TestAPinCannotNameAProviderThatDoesNotExist(t *testing.T) {
	cfg := config.Default()
	cfg.CassetteProvider = map[string]string{"feature": "anthropick"}
	if err := cfg.Resolve(); err == nil {
		t.Fatal("a pin naming an unconfigured provider was accepted")
	}
}

// Two members reaching one cassette at the same moment must get one store.
//
// A store holds the cursor into its own script, so a second store for the same
// cassette would replay it from the beginning halfway through a session — and
// under record, two stores appending to one index would each think they were
// writing step 1. Opening is therefore cached, and the cache has to hold under
// the concurrency that makes several agents worth serving at all.
func TestConcurrentFirstRequestsOpenOneStore(t *testing.T) {
	root := t.TempDir()
	var opens atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(up.Close)

	cfg := config.Default()
	cfg.Providers["anthropic"] = &config.Provider{BaseURL: up.URL}
	cfg.Providers["openai"] = &config.Provider{BaseURL: up.URL}
	if err := cfg.Resolve(); err != nil {
		t.Fatal(err)
	}
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), online)
	s = s.WithOpener(func(name string) (*cassette.Store, error) {
		opens.Add(1)
		return cassette.OpenStore(filepath.Join(root, name), "test",
			cfg.Normalize.Version, func() int64 { return 0 })
	})
	s.now = func() time.Time { return time.Unix(0, 0).UTC() }

	const members = 12
	var wg sync.WaitGroup
	for i := range members {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":"turn %d"}]}`, i)
			r := httptest.NewRequest(http.MethodPost, "/c/shared/v1/messages", strings.NewReader(body))
			s.ServeHTTP(httptest.NewRecorder(), r)
		}(i)
	}
	wg.Wait()

	if n := opens.Load(); n != 1 {
		t.Errorf("the cassette was opened %d times, want 1 — a second store restarts the script", n)
	}
	store, err := s.storeFor("shared")
	if err != nil || store == nil {
		t.Fatalf("storeFor: %v", err)
	}
	entries, err := store.Cassette().Entries()
	if err != nil {
		t.Fatal(err)
	}
	// Every turn is its own step, numbered once: two stores appending would
	// have collided on the sequence and lost entries.
	if len(entries) != members {
		t.Fatalf("recorded %d steps, want %d", len(entries), members)
	}
	seen := map[int]bool{}
	for _, e := range entries {
		if seen[e.Seq] {
			t.Errorf("sequence %d was written twice", e.Seq)
		}
		seen[e.Seq] = true
	}
}
