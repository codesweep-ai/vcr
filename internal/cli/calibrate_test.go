package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codesweep-ai/vcr/internal/cassette"
	"github.com/codesweep-ai/vcr/internal/config"
)

// The turn that cost three rounds of recording, replaying and diffing by hand:
// Codex times its own tool call and feeds the result into the next request.
func turn(wall, out string) string {
	return `{"model":"gpt-5.6-sol","input":[
	  {"role":"user","content":[{"type":"input_text","text":"list the files"}]},
	  {"call_id":"c1","type":"custom_tool_call","name":"exec","input":"ls internal"},
	  {"call_id":"c1","type":"custom_tool_call_output","output":[
	    {"type":"input_text","text":"Script completed\nWall time ` + wall + ` seconds\n"},
	    {"type":"input_text","text":"` + out + `"}]}]}`
}

// recordAndMiss builds a cassette of one step and the miss a replay of it
// dumped, which is exactly what `replay --dump-misses` leaves behind.
func recordAndMiss(t *testing.T, recorded, live string) (*cassette.Cassette, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "build")
	s, err := cassette.OpenStore(dir, "test", 1, func() int64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.Append(cassette.Recording{
		Entry:   cassette.Entry{Method: "POST", Path: "/responses", Status: 200},
		Request: []byte(recorded), Response: []byte(`{"ok":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	misses := filepath.Join(t.TempDir(), "misses")
	if err := os.MkdirAll(misses, 0o755); err != nil {
		t.Fatal(err)
	}
	// Named after the step it was compared against, which is what lets these
	// be paired without guessing.
	if err := os.WriteFile(filepath.Join(misses, "0001.json"), []byte(live), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = e
	_ = time.Now
	return s.Cassette(), misses
}

// THE test for this command: it has to find, mechanically, the rule that was
// found by hand over three rounds of recording and diffing.
func TestCalibrateProposesThePathThatDiffered(t *testing.T) {
	c, misses := recordAndMiss(t, turn("0.2", "cassette\\ncli\\n"), turn("0.6", "cassette\\ncli\\n"))

	out := &bytes.Buffer{}
	if err := calibrate(out, c, misses, nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()

	// The proposal is config, and nothing else: this is the whole reason the
	// command is worth having.
	for _, want := range []string{"normalize:", "volatile:", "- input[].output[].text"} {
		if !strings.Contains(got, want) {
			t.Errorf("the proposal does not contain %q:\n%s", want, got)
		}
	}
	// With the evidence, because a path alone does not tell a reader whether it
	// is the world answering or the agent asking.
	if !strings.Contains(got, "Wall time 0.2") || !strings.Contains(got, "Wall time 0.6") {
		t.Errorf("the proposal carries no example to judge it by:\n%s", got)
	}
	// Generalized, so one difference seen at one turn covers every turn of it.
	if strings.Contains(got, "output[0]") || strings.Contains(got, "input[2]") {
		t.Errorf("the proposal names a concrete index rather than a rule:\n%s", got)
	}
}

// It proposes what is MISSING, not what is already in force. A run against a
// ruleset that already covers a path should have nothing to say about it.
func TestCalibrateIsSilentAboutWhatIsAlreadyDeclared(t *testing.T) {
	c, misses := recordAndMiss(t, turn("0.2", "a\\n"), turn("0.6", "a\\n"))

	out := &bytes.Buffer{}
	if err := calibrate(out, c, misses, []string{"input[].output[].text"}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "volatile:") {
		t.Errorf("a path already in force was proposed again:\n%s", got)
	}
	if !strings.Contains(got, "already aligns") {
		t.Errorf("the run reports nothing about having found nothing:\n%s", got)
	}
}

// A difference no rule can cover must not be dressed up as one. An item added
// or gone means the request is built differently, and declaring the list it
// happened in would blank the list — for a prompt, that is the prompt.
func TestCalibrateDoesNotProposeARuleForAShapeDifference(t *testing.T) {
	c, misses := recordAndMiss(t,
		`{"input":[{"content":[{"text":"a"},{"text":"plugins"},{"text":"b"}]}]}`,
		`{"input":[{"content":[{"text":"a"},{"text":"b"}]}]}`)

	out := &bytes.Buffer{}
	if err := calibrate(out, c, misses, nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "volatile:") {
		t.Errorf("a rule was proposed for a request built differently:\n%s", got)
	}
	for _, want := range []string{"no rule can cover", "3 items vs 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("the difference is not explained as unproposable (%q):\n%s", want, got)
		}
	}
}

// A request that matched no step carries no proposal, and the run says so: a
// reader who assumed the output covered every miss would go looking for a rule
// that was never printed.
func TestCalibrateSaysWhatItCouldNotPair(t *testing.T) {
	c, misses := recordAndMiss(t, turn("0.2", "a\\n"), turn("0.2", "a\\n"))
	if err := os.WriteFile(filepath.Join(misses, "unpaired-abc123def456.json"),
		[]byte(`{"input":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	if err := calibrate(out, c, misses, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "matched no step") {
		t.Errorf("an unpairable request was passed over in silence:\n%s", out)
	}
}

// What calibrate prints has to BE configuration, not look like it.
//
// The proposal is meant to be pasted, so it is fed back through the real parser
// here — which rejects unknown fields, and is what caught an earlier version
// emitting `- path: x` for a setting that takes a plain list of paths.
func TestCalibrateOutputParsesAsConfig(t *testing.T) {
	c, misses := recordAndMiss(t, turn("0.2", "a\\n"), turn("0.6", "a\\n"))

	out := &bytes.Buffer{}
	if err := calibrate(out, c, misses, nil); err != nil {
		t.Fatal(err)
	}
	// Everything from `normalize:` on is the proposal. Locate it before
	// slicing: a missing key is a real failure, and slicing on Index's -1
	// would report it as an opaque bounds panic.
	got := out.String()
	start := strings.Index(got, "normalize:")
	if start < 0 {
		t.Fatalf("calibrate proposed no normalize block:\n%s", got)
	}
	yaml := got[start:]

	path := filepath.Join(t.TempDir(), "proposed.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the proposal does not parse as config: %v\n%s", err, yaml)
	}
	if got := cfg.Normalize.VolatilePaths(); len(got) != 1 || got[0] != "input[].output[].text" {
		t.Errorf("parsed volatile = %v, want the proposed path", got)
	}
	// And it does what it claims: the turn that missed now aligns.
	rec, err := os.ReadFile(c.RequestPath(1))
	if err != nil {
		t.Fatal(err)
	}
	live, err := os.ReadFile(filepath.Join(misses, "0001.json"))
	if err != nil {
		t.Fatal(err)
	}
	al, err := cassette.Align(rec, live, cassette.Rules(cfg.Normalize.VolatilePaths()))
	if err != nil {
		t.Fatal(err)
	}
	if !al.Matches() {
		t.Errorf("the proposed rule does not make the turn align: %+v", al)
	}
}
