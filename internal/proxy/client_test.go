package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/codesweep-ai/vcr/internal/config"
)

func forwardServer(t *testing.T, clients []*config.Client, upstream http.HandlerFunc) (*Server, *logBuffer) {
	t.Helper()
	s, logs := newTestServer(t, online, clients, upstream)
	return s, logs
}

// Identity from the connection, not the credential. Verified against the real
// client: Claude Code 2.1.219 with a base URL of http://host:8080/c/feature
// issues POST /c/feature/v1/messages?beta=true.
func TestPathPrefixIdentifiesTheClientAndIsStrippedUpstream(t *testing.T) {
	var gotPath string
	clients := []*config.Client{
		{Label: "feature.default", Match: config.ClientMatch{PathPrefix: "/c/feature"}},
		{Label: "ci-run-88", Match: config.ClientMatch{PathPrefix: "/c/ci"}},
	}
	s, _ := forwardServer(t, clients, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	})

	for _, tc := range []struct{ path, label string }{
		{"/c/feature/v1/messages", "feature.default"},
		{"/c/ci/v1/messages", "ci-run-88"},
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
		if n := s.Snapshot().ByLabel[tc.label]; n == 0 {
			t.Errorf("%s: not attributed to %s (labels: %v)", tc.path, tc.label, s.Snapshot().ByLabel)
		}
	}
	// And the surface is still classified correctly after the strip, or the
	// prefix would have quietly turned every request into an unknown path.
	if n := s.Snapshot().BySurface["anthropic.messages"]; n != 2 {
		t.Errorf("surface counts = %v, want both requests classified", s.Snapshot().BySurface)
	}
}

// A base URL missing its prefix is the mistake this design invites, and it must
// not look like success. Bucketing it as "unknown" would let a mistyped client
// land a client's traffic in the wrong cassette, or in none.
func TestUnmatchedPathIsRejectedNotSilentlyBucketed(t *testing.T) {
	reached := false
	clients := []*config.Client{{Label: "feature", Match: config.ClientMatch{PathPrefix: "/c/feature"}}}
	s, logs := forwardServer(t, clients, func(w http.ResponseWriter, r *http.Request) { reached = true })

	w := post(t, s, "/v1/messages", map[string]string{"authorization": "Bearer x"}, `{}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if reached {
		t.Error("an unattributable request reached upstream")
	}
	// The error names what it should have said, because the person reading it
	// is the person who mistyped a base URL.
	if !strings.Contains(w.Body.String(), "/c/feature") {
		t.Errorf("error does not name the configured prefixes: %s", w.Body)
	}
	if !strings.Contains(logs.String(), "no client matches") {
		t.Errorf("not logged: %s", logs)
	}
	if s.Snapshot().Unmatched != 1 {
		t.Errorf("Unmatched = %d, want 1", s.Snapshot().Unmatched)
	}
}

// Prefix matching is on segment boundaries: /c/feat must not claim traffic
// addressed to a client called /c/feature.
func TestPrefixMatchingIsOnSegmentBoundaries(t *testing.T) {
	clients := []*config.Client{{Label: "feat", Match: config.ClientMatch{PathPrefix: "/c/feat"}}}
	s, _ := forwardServer(t, clients, func(w http.ResponseWriter, r *http.Request) {})

	if w := post(t, s, "/c/feature/v1/messages", map[string]string{}, `{}`); w.Code != http.StatusNotFound {
		t.Errorf("/c/feature matched the client /c/feat: status = %d", w.Code)
	}
	if w := post(t, s, "/c/feat/v1/messages", map[string]string{}, `{}`); w.Code != http.StatusOK {
		t.Errorf("/c/feat did not match its own client: status = %d", w.Code)
	}
}

// With no clients configured, everything still works — the simple deployment
// pays nothing for a feature it is not using.
func TestNoClientsConfiguredServesEverythingAsDefault(t *testing.T) {
	s, _ := forwardServer(t, nil, func(w http.ResponseWriter, r *http.Request) {})
	if w := post(t, s, "/v1/messages", map[string]string{}, `{}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if n := s.Snapshot().ByLabel["default"]; n != 1 {
		t.Errorf("ByLabel = %v, want one request under \"default\"", s.Snapshot().ByLabel)
	}
}
