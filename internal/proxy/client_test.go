package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// forwardServer is a recording session over one upstream: the default provider
// set, both entries pointing at it. A test that needs them to differ builds its
// own with newTestServer.
func forwardServer(t *testing.T, upstream http.HandlerFunc) (*Server, *logBuffer) {
	t.Helper()
	return newTestServer(t, online, nil, upstream)
}

// Identity from the connection, not the credential, and nothing declares it.
// Verified against the real client: Claude Code 2.1.232 with a base URL of
// http://host:8080/c/anthropic/feature issues
// POST /c/anthropic/feature/v1/messages?beta=true.
func TestPrefixNamesTheCassetteAndIsStrippedUpstream(t *testing.T) {
	var gotPath string
	s, _ := forwardServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	})

	for _, tc := range []struct{ path, cassette string }{
		{"/c/anthropic/feature/v1/messages", "feature"},
		{"/c/anthropic/ci-run-88/v1/messages", "ci-run-88"},
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
	s, logs := forwardServer(t, func(w http.ResponseWriter, r *http.Request) { reached = true })

	for _, path := range []string{
		"/c/anthropic/../v1/messages", "/c/anthropic/..%2F..%2Fetc/v1/messages", "/c/anthropic//v1/messages",
		// The provider segment is the other door to the same place.
		"/c/../feature/v1/messages", "/c//feature/v1/messages",
	} {
		w := post(t, s, path, map[string]string{}, `{}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body %s)", path, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), "bad_prefix") {
			t.Errorf("%s: error type is not bad_prefix: %s", path, w.Body)
		}
	}
	if reached {
		t.Error("a request naming an unusable cassette reached upstream")
	}
	if !strings.Contains(logs.String(), "does not name a usable provider and cassette") {
		t.Errorf("not logged: %s", logs)
	}
	if n := s.Snapshot().UnknownCassette; n != 5 {
		t.Errorf("UnknownCassette = %d, want 5", n)
	}
}

// A base URL that stops before it has named both segments is a composition
// mistake, and it is refused rather than guessed at.
func TestAnIncompletePrefixIsRefused(t *testing.T) {
	s, _ := forwardServer(t, nil)
	for _, path := range []string{"/c/", "/c/feature", "/c/feature/"} {
		if w := post(t, s, path, map[string]string{}, `{}`); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body %s)", path, w.Code, w.Body)
		}
	}
}

// Splitting is on segment boundaries, so two cassettes whose names share a
// prefix stay separate.
func TestPrefixMatchingIsOnSegmentBoundaries(t *testing.T) {
	s, _ := forwardServer(t, func(w http.ResponseWriter, r *http.Request) {})

	for _, tc := range []struct{ path, cassette string }{
		{"/c/anthropic/feat/v1/messages", "feat"},
		{"/c/anthropic/feature/v1/messages", "feature"},
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

// A request whose base URL never named a cassette is refused, and does not
// reach a provider on the way.
//
// This is the mistake the design invites, and it must not look like success:
// absorbing it into a default is how a base URL missing its prefix records into
// somebody else's cassette, or into none, and says nothing until the run that
// depends on it fails.
func TestAPathThatNamesNoCassetteIsRefusedAndDoesNotDial(t *testing.T) {
	reached := false
	s, logs := forwardServer(t, func(w http.ResponseWriter, r *http.Request) { reached = true })

	for _, path := range []string{"/v1/messages", "/api/hello", "/cx/build/v1/messages"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 (body %s)", path, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), "no_prefix") {
			t.Errorf("%s: error type is not no_cassette: %s", path, w.Body)
		}
		// The reply says what the base URL should have ended in, because the
		// person reading it is the person who wrote that URL.
		if !strings.Contains(w.Body.String(), "/c/") {
			t.Errorf("%s: the error does not name the prefix: %s", path, w.Body)
		}
	}
	if reached {
		t.Error("a request naming no cassette reached upstream")
	}
	if !strings.Contains(logs.String(), "does not name a usable provider and cassette") {
		t.Errorf("not logged: %s", logs)
	}
	if n := s.Snapshot().UnknownCassette; n != 3 {
		t.Errorf("UnknownCassette = %d, want 3", n)
	}
}

// The prefix takes every request on it, whatever the path. This is what a
// bodiless probe follows: nothing in `GET /models` says which provider it was
// going to.
func TestThePrefixTakesEveryPathOnTheCassette(t *testing.T) {
	var seen []string
	s, _ := forwardServer(t,
		func(w http.ResponseWriter, r *http.Request) { seen = append(seen, r.URL.Path) })

	for _, path := range []string{"/c/openai/codex-run/models", "/c/openai/codex-run/v1/responses"} {
		if w := post(t, s, path, map[string]string{}, `{}`); w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (body %s)", path, w.Code, w.Body)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("upstream saw %v, want both requests", seen)
	}
}
