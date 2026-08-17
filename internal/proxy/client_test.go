package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/codesweep-ai/vcr/internal/config"
)

func forwardServer(t *testing.T, tune func(*config.Config), upstream http.HandlerFunc) (*Server, *logBuffer) {
	t.Helper()
	return newTestServer(t, online, tune, upstream)
}

// Identity from the connection, not the credential, and nothing declares it.
// Verified against the real client: Claude Code 2.1.232 with a base URL of
// http://host:8080/c/feature issues POST /c/feature/v1/messages?beta=true.
func TestPrefixNamesTheCassetteAndIsStrippedUpstream(t *testing.T) {
	var gotPath string
	s, _ := forwardServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	})

	for _, tc := range []struct{ path, cassette string }{
		{"/c/feature/v1/messages", "feature"},
		{"/c/ci-run-88/v1/messages", "ci-run-88"},
	} {
		gotPath = ""
		w := post(t, s, tc.path, map[string]string{"authorization": "Bearer x"}, `{}`)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (body %s)", tc.path, w.Code, w.Body)
		}
		// The provider must see its own path, not cs-vcr's addressing.
		if gotPath != "/v1/messages" {
			t.Errorf("%s: upstream saw %q, want /v1/messages", tc.path, gotPath)
		}
		if n := s.Snapshot().ByCassette[tc.cassette]; n == 0 {
			t.Errorf("%s: not attributed to %s (cassettes: %v)",
				tc.path, tc.cassette, s.Snapshot().ByCassette)
		}
	}
	// And the surface is still classified correctly after the strip, or the
	// prefix would have quietly turned every request into an unknown path.
	if n := s.Snapshot().BySurface["anthropic.messages"]; n != 2 {
		t.Errorf("surface counts = %v, want both requests classified", s.Snapshot().BySurface)
	}
}

// A name that is not a name must be refused rather than joined to a path. The
// name arrives in a URL and becomes a directory, so this is the door between a
// request and the filesystem.
func TestATraversingCassetteNameIsRefusedAndDoesNotDial(t *testing.T) {
	reached := false
	s, logs := forwardServer(t, nil, func(w http.ResponseWriter, r *http.Request) { reached = true })

	for _, path := range []string{"/c/../v1/messages", "/c/..%2F..%2Fetc/v1/messages", "/c//v1/messages"} {
		w := post(t, s, path, map[string]string{}, `{}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body %s)", path, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), "bad_cassette_name") {
			t.Errorf("%s: error type is not bad_cassette_name: %s", path, w.Body)
		}
	}
	if reached {
		t.Error("a request naming an unusable cassette reached upstream")
	}
	if !strings.Contains(logs.String(), "does not name a usable cassette") {
		t.Errorf("not logged: %s", logs)
	}
	if n := s.Snapshot().UnknownCassette; n != 3 {
		t.Errorf("UnknownCassette = %d, want 3", n)
	}
}

// A base URL that stops at the bare prefix is a composition mistake, and it
// must not quietly become the session's cassette — that is how one test's
// traffic lands in another test's recording.
func TestABarePrefixIsRefusedRatherThanFallingBack(t *testing.T) {
	s, _ := forwardServer(t, func(c *config.Config) { c.Cassette = "session" }, nil)
	w := post(t, s, "/c/", map[string]string{}, `{}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body)
	}
}

// Splitting is on segment boundaries, so two cassettes whose names share a
// prefix stay separate.
func TestPrefixMatchingIsOnSegmentBoundaries(t *testing.T) {
	s, _ := forwardServer(t, nil, func(w http.ResponseWriter, r *http.Request) {})

	for _, tc := range []struct{ path, cassette string }{
		{"/c/feat/v1/messages", "feat"},
		{"/c/feature/v1/messages", "feature"},
	} {
		if w := post(t, s, tc.path, map[string]string{}, `{}`); w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", tc.path, w.Code)
		}
		if n := s.Snapshot().ByCassette[tc.cassette]; n != 1 {
			t.Errorf("%s: attributed to %v, want one request under %q",
				tc.path, s.Snapshot().ByCassette, tc.cassette)
		}
	}
}

// With no prefix and no session cassette everything still works — the simple
// deployment pays nothing for a feature it is not using.
func TestNoPrefixAndNoCassetteStillProxies(t *testing.T) {
	s, _ := forwardServer(t, nil, func(w http.ResponseWriter, r *http.Request) {})
	if w := post(t, s, "/v1/messages", map[string]string{}, `{}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if n := s.Snapshot().ByCassette["-"]; n != 1 {
		t.Errorf("ByCassette = %v, want one request under \"-\"", s.Snapshot().ByCassette)
	}
}

// A request with no prefix falls back to the session's cassette, which is what
// the single-agent deployment and the headerless startup probes both rely on.
func TestNoPrefixFallsBackToTheSessionCassette(t *testing.T) {
	s, _ := forwardServer(t, func(c *config.Config) { c.Cassette = "session" },
		func(w http.ResponseWriter, r *http.Request) {})
	if w := post(t, s, "/v1/messages", map[string]string{}, `{}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if n := s.Snapshot().ByCassette["session"]; n != 1 {
		t.Errorf("ByCassette = %v, want one request under \"session\"", s.Snapshot().ByCassette)
	}
}

// A pinned provider takes every request on its cassette, whatever the path.
// This is what a bodiless probe follows: nothing in `GET /models` says which
// provider it was going to.
func TestAPinnedProviderTakesEveryPathOnItsCassette(t *testing.T) {
	var seen []string
	s, _ := forwardServer(t, func(c *config.Config) {
		c.DefaultProvider = "anthropic"
		c.CassetteProvider = map[string]string{"codex-run": "openai"}
	}, func(w http.ResponseWriter, r *http.Request) { seen = append(seen, r.URL.Path) })

	for _, path := range []string{"/c/codex-run/models", "/c/codex-run/v1/responses"} {
		if w := post(t, s, path, map[string]string{}, `{}`); w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (body %s)", path, w.Code, w.Body)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("upstream saw %v, want both requests", seen)
	}
}
