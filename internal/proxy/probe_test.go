package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/vcr/internal/cassette"
	"github.com/codesweep-ai/vcr/internal/config"
)

// A request for a cassette's bare address is somebody asking whether the proxy
// is up: a sandbox's doctor command, a person with curl. It is not the session,
// so it must not fail one. Measured on a shared host, where a doctor run during
// a replay that served every step left it with misses and exit status 4.
func TestAProbeOfTheBareAddressIsNotAMiss(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "probed")
	const first = `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"one"}]}`
	const second = `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"two"}]}`

	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	post(t, rec, "/v1/messages", nil, first)
	post(t, rec, "/v1/messages", nil, second)

	rep, logs := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replay contacted the provider")
	})
	post(t, rep, "/v1/messages", nil, first)
	// Both spellings of the bare address, and the bodiless method a checker may
	// prefer. In the middle of the session, because that is when it hurts.
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, ""}, {http.MethodGet, "/"}, {http.MethodHead, ""},
	} {
		w := do(t, rep, probe.method, probe.path)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %q: status = %d, want 200 (body %s)", probe.method, probe.path, w.Code, w.Body)
		}
	}
	// The session carries on from where it was: a probe moves nothing.
	if w := post(t, rep, "/v1/messages", nil, second); w.Code != http.StatusOK {
		t.Fatalf("the step after the probes: status = %d (body %s)\nlogs: %s", w.Code, w.Body, logs)
	}

	st := rep.Snapshot()
	if st.Misses != 0 || st.Rejected != 0 {
		t.Errorf("misses = %d, rejected = %d, want 0 and 0", st.Misses, st.Rejected)
	}
	if st.Probes != 3 {
		t.Errorf("probes = %d, want 3", st.Probes)
	}
	// Apart from requests, so that requests still equals replayed on a session
	// that served everything it was asked.
	if st.Requests != 2 || st.Replayed != 2 || st.OutOfOrder != 0 {
		t.Errorf("requests = %d, replayed = %d, out of order = %d, want 2, 2 and 0",
			st.Requests, st.Replayed, st.OutOfOrder)
	}
}

// The answer says which half of cs-vcr is listening and for which cassette, so
// a checker can tell a replay from a recording without reading a log.
func TestAProbeIsToldWhatIsListening(t *testing.T) {
	rep, _ := cassetteServer(t, filepath.Join(t.TempDir(), "named"), offline, func(http.ResponseWriter, *http.Request) {
		t.Error("replay contacted the provider")
	})
	w := do(t, rep, http.MethodGet, "")
	for _, want := range []string{`"mode":"replay"`, `"cassette":"` + testCassette + `"`, `"source":"cs-vcr"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("body = %s, want it to hold %s", w.Body, want)
		}
	}
}

// The recording half of the same defect. An unrecognized path is forwarded and
// recorded, which is right for a path the agent asked for and wrong for this
// one: the provider would be asked for its front page with nobody's credential,
// and the cassette would gain a step no replay ever makes.
func TestAProbeWhileRecordingReachesNoProviderAndAddsNoStep(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "probed-recording")
	rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			t.Error("the probe reached the provider")
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	if w := do(t, rec, http.MethodGet, ""); w.Code != http.StatusOK {
		t.Fatalf("probe: status = %d, want 200", w.Code)
	}
	post(t, rec, "/v1/messages", nil, `{"model":"claude-sonnet-5"}`)

	st := rec.Snapshot()
	if st.Recorded != 1 || st.Upstream != 1 || st.Probes != 1 {
		t.Errorf("recorded = %d, upstream = %d, probes = %d, want 1, 1 and 1", st.Recorded, st.Upstream, st.Probes)
	}
	c, err := cassette.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := c.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "/v1/messages" {
		t.Errorf("entries = %+v, want the one prompt and no probe", entries)
	}
}

// A probe answers for the cassette it named. One the store does not hold is the
// mistyped base URL a checker exists to find, so it stays an error.
func TestAProbeOfAnUnknownCassetteIsStillRefused(t *testing.T) {
	cfg := config.Default()
	if err := cfg.ResolveOffline(); err != nil {
		t.Fatal(err)
	}
	rep := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), offline).
		WithOpener(func(string) (*cassette.Store, error) { return nil, ErrNoSuchCassette })
	w := do(t, rep, http.MethodGet, "")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "unknown_cassette") {
		t.Errorf("status = %d body = %s, want 404 unknown_cassette", w.Code, w.Body)
	}
	if st := rep.Snapshot(); st.Probes != 0 || st.UnknownCassette != 1 {
		t.Errorf("probes = %d, unknown cassette = %d, want 0 and 1", st.Probes, st.UnknownCassette)
	}
}

// Only the bodiless methods. A POST to the bare address is a client whose base
// URL lost its path, and answering that 200 would hide it.
func TestAPostToTheBareAddressIsNotAProbe(t *testing.T) {
	rep, _ := cassetteServer(t, filepath.Join(t.TempDir(), "posted"), offline, func(http.ResponseWriter, *http.Request) {
		t.Error("replay contacted the provider")
	})
	if w := post(t, rep, "", nil, `{}`); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want the miss a POST anywhere else gets", w.Code)
	}
	if st := rep.Snapshot(); st.Misses != 1 || st.Probes != 0 {
		t.Errorf("misses = %d, probes = %d, want 1 and 0", st.Misses, st.Probes)
	}
}
